package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	dingcard "github.com/alibabacloud-go/dingtalk/card_1_0"
	util "github.com/alibabacloud-go/tea-utils/v2/service"
	"github.com/google/uuid"
	"github.com/noble-gase/argon/userlock"
	"github.com/noble-gase/neon/helper"
	"github.com/noble-gase/neon/httpkit"
	"github.com/redis/go-redis/v9"
	"github.com/tidwall/gjson"
)

type AccessToken struct {
	Token     string `json:"token"`
	ExpiredAt int64  `json:"expired_at"`
}

// fresh 判断 token 在余量 margin 下是否仍可用。
func (at AccessToken) fresh(margin time.Duration) bool {
	return at.Token != "" && at.ExpiredAt-time.Now().Unix() > int64(margin/time.Second)
}

type CardSender struct {
	clientId     string
	clientSecret string
	templateId   string

	// confirmTemplateId 是人工确认卡片使用的模板。它应当有两个按钮，回调时通过
	// "decision" 参数（"approve"/"reject"）或 action id
	// （"confirm_approve"/"confirm_reject"）上报决定。
	confirmTemplateId string

	tokenKey string

	card  *dingcard.Client
	reduc redis.UniversalClient

	// lock 跨实例串行化同一用户的消息处理，见 userlock 包。
	lock *userlock.Locker

	// tokenMu 保护以下字段并串行化刷新：并发调用只有一个真的去请求钉钉，
	// 其余等它写回缓存。
	tokenMu        sync.Mutex
	token          AccessToken
	lastFetchErr   error
	lastFetchErrAt time.Time

	// fetchToken 由构造函数设为 fetchAccessToken，测试可覆盖成假实现。
	fetchToken func(context.Context) (AccessToken, error)
}

// deliver 创建并投放一张卡片，返回它的 outTrackId。spaceType 取 "IM_ROBOT"
// （单聊）或 "IM_GROUP"（群聊，需设置 groupConvId）。
func (s *CardSender) deliver(ctx context.Context, outTrackId, templateId, spaceType, userId, groupConvId string, params map[string]string) (string, error) {
	accessToken, err := s.accessToken(ctx)
	if err != nil {
		return "", err
	}

	req := &dingcard.CreateAndDeliverRequest{
		CallbackType: new("STREAM"),
		CardData: &dingcard.CreateAndDeliverRequestCardData{
			CardParamMap: map[string]*string{},
		},
		CardTemplateId: new(templateId),
		OutTrackId:     new(outTrackId),
		UserId:         new(userId),
		UserIdType:     new(int32(1)),
	}
	if len(params) != 0 {
		for k, v := range params {
			req.CardData.CardParamMap[k] = new(v)
		}
	}

	switch spaceType {
	case "IM_GROUP":
		req.ImGroupOpenDeliverModel = &dingcard.CreateAndDeliverRequestImGroupOpenDeliverModel{
			RobotCode:  new(s.clientId),
			Recipients: []*string{new(userId)},
		}
		req.ImGroupOpenSpaceModel = &dingcard.CreateAndDeliverRequestImGroupOpenSpaceModel{SupportForward: new(true)}
		req.OpenSpaceId = new(fmt.Sprintf("dtv1.card//im_group.%s", groupConvId))
	default: // 单聊 IM_ROBOT
		req.ImRobotOpenDeliverModel = &dingcard.CreateAndDeliverRequestImRobotOpenDeliverModel{
			SpaceType: new("IM_ROBOT"),
			RobotCode: new(s.clientId),
		}
		req.ImRobotOpenSpaceModel = &dingcard.CreateAndDeliverRequestImRobotOpenSpaceModel{SupportForward: new(true)}
		req.OpenSpaceId = new(fmt.Sprintf("dtv1.card//im_robot.%s", userId))
	}

	headers := &dingcard.CreateAndDeliverHeaders{
		XAcsDingtalkAccessToken: new(accessToken),
	}

	if _, err = s.card.CreateAndDeliverWithOptions(req, headers, &util.RuntimeOptions{}); err != nil {
		return "", err
	}
	return outTrackId, nil
}

// CreateAndDeliverRobot 投放「机器人单聊」卡片，返回 outTrackId。
// 卡片正文是流式变量，创建时不设初值（会被忽略），由调用方 StreamingUpdate 推送。
func (s *CardSender) CreateAndDeliverRobot(ctx context.Context, userId string) (string, error) {
	return s.deliver(ctx, uuid.New().String(), s.templateId, "IM_ROBOT", userId, "", nil)
}

// CreateAndDeliverGroup 投放「群聊」卡片，返回 outTrackId。
// 卡片正文是流式变量，创建时不设初值（会被忽略），由调用方 StreamingUpdate 推送。
func (s *CardSender) CreateAndDeliverGroup(ctx context.Context, userId, conversationId string) (string, error) {
	return s.deliver(ctx, uuid.New().String(), s.templateId, "IM_GROUP", userId, conversationId, nil)
}

// NewOutTrackId 预生成卡片 ID，让确认状态可以先落库再投卡：反过来的话，
// 投卡成功而落库失败会留下一张点了没反应的卡片。
func (s *CardSender) NewOutTrackId() string {
	return uuid.New().String()
}

// DeliverConfirm 用指定的 outTrackId 投放「确认」卡片（带同意/拒绝按钮）。
func (s *CardSender) DeliverConfirm(ctx context.Context, outTrackId string, meta msgMeta, content string) (string, error) {
	params := map[string]string{"content": content}

	if meta.convType == "2" { // 群聊
		return s.deliver(ctx, outTrackId, s.confirmTemplateId, "IM_GROUP", meta.userId, meta.groupConvId, params)
	}
	return s.deliver(ctx, outTrackId, s.confirmTemplateId, "IM_ROBOT", meta.userId, "", params)
}

// lockUser 跨实例串行化同一用户的消息处理：两条消息并发驱动同一个 ADK session
// 会让事件交错、待回答状态互相覆盖。语义见 userlock.Locker.Lock。
func (s *CardSender) lockUser(ctx context.Context, userId string) (context.Context, func(), error) {
	return s.lock.Lock(ctx, userId)
}

// StreamingUpdate 流式更新卡片内容（全量覆盖）
func (s *CardSender) StreamingUpdate(ctx context.Context, outTrackId, content string, finished bool) {
	accessToken, err := s.accessToken(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "[dingtalk card] load access_token failed", slog.String("outTrackId", outTrackId), slog.String("error", err.Error()))
		return
	}
	request := &dingcard.StreamingUpdateRequest{
		Content:    new(content),
		Guid:       new(uuid.New().String()),
		IsError:    new(false),
		IsFinalize: new(finished),
		IsFull:     new(true),
		Key:        new("content"),
		OutTrackId: new(outTrackId),
	}

	headers := &dingcard.StreamingUpdateHeaders{
		XAcsDingtalkAccessToken: new(accessToken),
	}

	_, err = s.card.StreamingUpdateWithOptions(request, headers, &util.RuntimeOptions{})
	if err != nil {
		slog.ErrorContext(ctx, "[dingtalk card] update stream failed", slog.String("outTrackId", outTrackId), slog.String("error", err.Error()))
	}
}

// tokenRefreshMargin 是提前刷新的余量：剩余有效期低于它时就地刷新，
// 避免拿着临期 token 发卡失败。
const tokenRefreshMargin = 10 * time.Minute

// tokenIOTimeout 限制锁内外部调用（Redis 读写 + 直连钉钉）的总时长。
// sync.Mutex 的等待者不响应各自 ctx 的取消，临界区必须自带上界：否则一次
// 挂起的刷新会握着锁陪调用方的 ctx 走完（最长可达 Bot 的运行超时），
// 把全进程的卡片投放和更新一起拖住。
const tokenIOTimeout = 10 * time.Second

// tokenRetryBackoff 是刷新失败后的静默期。端点挂起（而不是快速失败）时，每次
// 试探都要在锁内把 tokenIOTimeout 走满，并发调用会排成 10 秒一个的队列——哪怕
// 手里有可降级的旧 token。静默期内跳过全部 I/O，直接降级或复报上次的错误。
const tokenRetryBackoff = 30 * time.Second

// accessToken 按需返回可用的 access token：进程内缓存 → Redis（别的实例可能
// 刚刷新过）→ 直连钉钉刷新。没有后台刷新协程，也就没有需要 Close 的生命周期。
//
// 整个获取过程持 tokenMu 串行化：并发调用只有一个真的去刷新，其余等它写回
// 缓存；命中内存缓存时锁只握一瞬，未命中时锁内 I/O 由 tokenIOTimeout 封顶。
// 钉钉的 token 端点在有效期内对并发请求返回同一 token，跨实例的偶发重复刷新
// 无害，不需要分布式锁。
func (s *CardSender) accessToken(ctx context.Context) (string, error) {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()

	if s.token.fresh(tokenRefreshMargin) {
		return s.token.Token, nil
	}

	// 刷新失败后的静默期内不再试探端点，见 tokenRetryBackoff
	if s.lastFetchErr != nil && time.Since(s.lastFetchErrAt) < tokenRetryBackoff {
		if s.token.fresh(0) {
			return s.token.Token, nil
		}
		return "", s.lastFetchErr
	}

	// 与调用方的 ctx 解绑（只保留 trace 等值）并统一限时，见 tokenIOTimeout
	ioCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tokenIOTimeout)
	defer cancel()

	// 临期但未过期的 Redis token 留作降级候选：刚启动的实例内存缓存为空，
	// 刷新一旦失败它就是唯一还能用的凭据
	var stale AccessToken
	if at, err := s.redisToken(ioCtx); err == nil {
		if at.fresh(tokenRefreshMargin) {
			s.token = at
			return at.Token, nil
		}
		stale = at
	} else if !errors.Is(err, redis.Nil) {
		// Redis 故障不中止：还有直连刷新兜底
		slog.WarnContext(ctx, "[dingtalk card] redis get access_token failed", slog.String("error", err.Error()))
	}

	at, err := s.fetchToken(ioCtx)
	if err != nil {
		s.lastFetchErr, s.lastFetchErrAt = err, time.Now()
		// 刷新失败时，还没真正过期的旧 token（内存或 Redis 里的）仍可降级使用
		if !s.token.fresh(0) && stale.fresh(0) {
			s.token = stale
		}
		if s.token.fresh(0) {
			slog.WarnContext(ctx, "[dingtalk card] refresh failed, using cached access_token", slog.String("error", err.Error()))
			return s.token.Token, nil
		}
		return "", err
	}

	s.lastFetchErr = nil
	s.token = at
	s.storeRedisToken(ioCtx, at)
	return at.Token, nil
}

func (s *CardSender) redisToken(ctx context.Context) (AccessToken, error) {
	if s.reduc == nil {
		return AccessToken{}, redis.Nil
	}

	str, err := s.reduc.Get(ctx, s.tokenKey).Result()
	if err != nil {
		return AccessToken{}, err
	}
	var at AccessToken
	if err := json.Unmarshal([]byte(str), &at); err != nil {
		return AccessToken{}, err
	}
	return at, nil
}

// fetchAccessToken 直连钉钉换取新 token。
func (s *CardSender) fetchAccessToken(ctx context.Context) (AccessToken, error) {
	resp, err := httpkit.Client().R().
		SetContext(ctx).
		SetBody(helper.X{
			"appKey":    s.clientId,
			"appSecret": s.clientSecret,
		}).
		Post("https://api.dingtalk.com/v1.0/oauth2/accessToken")
	if err != nil {
		return AccessToken{}, fmt.Errorf("refresh access token: %w", err)
	}
	if !resp.IsSuccess() {
		return AccessToken{}, fmt.Errorf("refresh access token: %s", resp.Status())
	}

	ret := gjson.ParseBytes(resp.Body())
	token := ret.Get("accessToken").String()
	if token == "" {
		return AccessToken{}, errors.New("refresh access token: empty accessToken in response")
	}
	expireIn := ret.Get("expireIn").Int()
	if expireIn <= 0 {
		expireIn = 7200
	}
	return AccessToken{Token: token, ExpiredAt: time.Now().Unix() + expireIn}, nil
}

// storeRedisToken 把新 token 写回 Redis 供其它实例复用，失败只降级为各自刷新。
func (s *CardSender) storeRedisToken(ctx context.Context, at AccessToken) {
	if s.reduc == nil {
		return
	}

	b, err := json.Marshal(at)
	if err != nil {
		return
	}
	// TTL 对齐真实有效期，token 不会以过期状态残留
	ttl := time.Until(time.Unix(at.ExpiredAt, 0))
	if ttl <= 0 {
		return
	}
	if err := s.reduc.Set(ctx, s.tokenKey, string(b), ttl).Err(); err != nil {
		slog.WarnContext(ctx, "[dingtalk card] redis set access_token failed", slog.String("error", err.Error()))
	}
}

func NewCardSender(cfg *Config, uc redis.UniversalClient) (*CardSender, error) {
	client, err := dingcard.NewClient(&openapi.Config{
		Protocol: new("https"),
		RegionId: new("central"),
	})
	if err != nil {
		return nil, err
	}

	confirmTemplateId := cfg.CardTemplateId
	if cfg.ConfirmCard != nil && cfg.ConfirmCard.TemplateId != "" {
		confirmTemplateId = cfg.ConfirmCard.TemplateId
	}

	s := &CardSender{
		clientId:     cfg.ClientId,
		clientSecret: cfg.ClientSecret,
		templateId:   cfg.CardTemplateId,

		confirmTemplateId: confirmTemplateId,

		tokenKey: fmt.Sprintf("adk:access_token:dingtalk:%s", cfg.ClientId),

		lock: userlock.New(uc, userlock.Config{
			Prefix: fmt.Sprintf("adk:userlock:dingtalk:%s", cfg.ClientId),
			Wait:   cfg.LockWait,
		}),

		card:  client,
		reduc: uc,
	}
	s.fetchToken = s.fetchAccessToken

	// 启动时验证一次凭据，让配置错误在部署时暴露，而不是等第一条消息才失败。
	// 时限由 accessToken 内部的 tokenIOTimeout 保证（它与外部 ctx 的期限解绑）。
	if _, err := s.accessToken(context.Background()); err != nil {
		return nil, fmt.Errorf("initialize DingTalk access token: %w", err)
	}

	return s, nil
}
