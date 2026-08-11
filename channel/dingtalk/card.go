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
	"github.com/noble-gase/neon/helper"
	"github.com/noble-gase/neon/httpkit"
	"github.com/noble-gase/neon/redlock"
	"github.com/redis/go-redis/v9"
	"github.com/tidwall/gjson"
)

type AccessToken struct {
	Token     string `json:"token"`
	ExpiredAt int64  `json:"expired_at"`
}

type CardSender struct {
	clientId     string
	clientSecret string
	templateId   string

	// confirmTemplateId 是人工确认卡片使用的模板。它应当有两个按钮，回调时通过
	// "decision" 参数（"approve"/"reject"）或 action id
	// （"confirm_approve"/"confirm_reject"）上报决定。
	confirmTemplateId string

	lockKey  string
	tokenKey string

	card  *dingcard.Client
	reduc redis.UniversalClient

	// lock*Override 仅用于测试提速，为零时用 default* 常量。
	lockTTLOverride   time.Duration
	lockRenewOverride time.Duration
	lockRetryOverride time.Duration

	done      chan struct{}
	cancel    context.CancelFunc
	closeOnce sync.Once
}

// Close 停止后台刷新 access token 的 goroutine，可以安全地重复调用。
func (s *CardSender) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.cancel()
	})
}

// deliver 创建并投放一张卡片，返回它的 outTrackId。spaceType 取 "IM_ROBOT"
// （单聊）或 "IM_GROUP"（群聊，需设置 groupConvId）。
func (s *CardSender) deliver(ctx context.Context, outTrackId, templateId, spaceType, userId, groupConvId string, params map[string]string) (string, error) {
	accessToken, err := s.loadAccessToken(ctx)
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

// StreamingUpdate 流式更新卡片内容（全量覆盖）
func (s *CardSender) StreamingUpdate(ctx context.Context, outTrackId, content string, finished bool) {
	accessToken, err := s.loadAccessToken(ctx)
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

func (s *CardSender) loadAccessToken(ctx context.Context) (string, error) {
	if s.reduc == nil {
		return "", errors.New("missing redis client")
	}

	str, err := s.reduc.Get(ctx, s.tokenKey).Result()
	if err != nil {
		return "", err
	}
	return gjson.Get(str, "token").String(), nil
}

func (s *CardSender) refreshAccessToken(ctx context.Context) {
	lock := redlock.New(s.reduc, s.lockKey, 10*time.Second)
	if err := lock.Acquire(ctx); err != nil {
		return
	}
	defer lock.Release(ctx)

	str, err := s.reduc.Get(ctx, s.tokenKey).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		slog.ErrorContext(ctx, "[dingtalk card] redis get access_token failed", slog.String("key", s.tokenKey), slog.String("error", err.Error()))
		return
	}
	if len(str) != 0 {
		expiredAt := gjson.Get(str, "expired_at").Int()
		if expiredAt-time.Now().Unix() > 600 {
			return
		}
	}

	resp, err := httpkit.Client().R().
		SetContext(ctx).
		SetBody(helper.X{
			"appKey":    s.clientId,
			"appSecret": s.clientSecret,
		}).
		Post("https://api.dingtalk.com/v1.0/oauth2/accessToken")
	if err != nil {
		slog.ErrorContext(ctx, "[dingtalk card] refresh access_token failed", slog.String("error", err.Error()))
		return
	}

	slog.InfoContext(ctx, "[dingtalk card] refresh access_token", slog.String("response", resp.String()))

	if !resp.IsSuccess() {
		slog.ErrorContext(ctx, "[dingtalk card] refresh access_token failed", slog.String("error", resp.Status()))
		return
	}

	ret := gjson.ParseBytes(resp.Body())
	expireIn := ret.Get("expireIn").Int()
	at := AccessToken{
		Token:     ret.Get("accessToken").String(),
		ExpiredAt: time.Now().Unix() + expireIn,
	}
	b, _ := json.Marshal(at)
	// 设置 TTL，避免刷新协程停止后旧 token 永久残留
	ttl := time.Duration(expireIn) * time.Second
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	if err := s.reduc.Set(ctx, s.tokenKey, string(b), ttl).Err(); err != nil {
		slog.ErrorContext(ctx, "[dingtalk card] redis set access_token failed", slog.String("key", s.tokenKey), slog.String("value", string(b)), slog.String("error", err.Error()))
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

		lockKey:  fmt.Sprintf("adk:mutex:dingtalk:%s", cfg.ClientId),
		tokenKey: fmt.Sprintf("adk:access_token:dingtalk:%s", cfg.ClientId),

		card:  client,
		reduc: uc,

		done: make(chan struct{}),
	}

	initCtx, initCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer initCancel()

	s.refreshAccessToken(initCtx)
	if _, err = s.loadAccessToken(initCtx); err != nil {
		return nil, fmt.Errorf("initialize DingTalk access token: %w", err)
	}

	refreshCtx, refreshCancel := context.WithCancel(context.Background())
	s.cancel = refreshCancel

	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-ticker.C:
				s.refreshAccessToken(refreshCtx)
			}
		}
	}()

	return s, nil
}
