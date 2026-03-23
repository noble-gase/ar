package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	dingtalkcard "github.com/alibabacloud-go/dingtalk/card_1_0"
	util "github.com/alibabacloud-go/tea-utils/v2/service"
	"github.com/alibabacloud-go/tea/tea"
	"github.com/google/uuid"
	"github.com/noble-gase/ne/helper"
	"github.com/noble-gase/ne/redlock"
	"github.com/redis/go-redis/v9"
	"github.com/tidwall/gjson"
)

type AccessToken struct {
	Token     string `json:"token"`
	ExpiredAt int64  `json:"expired_at"`
}

// -------- 卡片 Client --------

type CardSender struct {
	clientId     string
	clientSecret string
	templateId   string

	lockKey  string
	tokenKey string

	card  *dingtalkcard.Client
	reduc redis.UniversalClient
}

// CreateAndDeliverRobot 投放「机器人单聊」卡片，返回 outTrackId
func (s *CardSender) CreateAndDeliverRobot(ctx context.Context, userId, initContent string) (string, error) {
	accessToken, err := s.loadAccessToken(ctx)
	if err != nil {
		return "", err
	}

	outTrackId := uuid.New().String()

	cardParamMap := map[string]*string{
		"content": tea.String(initContent),
	}

	req := &dingtalkcard.CreateAndDeliverRequest{
		CallbackType:   tea.String("STREAM"),
		CardData:       &dingtalkcard.CreateAndDeliverRequestCardData{CardParamMap: cardParamMap},
		CardTemplateId: tea.String(s.templateId),
		ImRobotOpenDeliverModel: &dingtalkcard.CreateAndDeliverRequestImRobotOpenDeliverModel{
			SpaceType: tea.String("IM_ROBOT"),
			RobotCode: tea.String(s.clientId),
		},
		ImRobotOpenSpaceModel: &dingtalkcard.CreateAndDeliverRequestImRobotOpenSpaceModel{
			SupportForward: tea.Bool(true),
		},
		OpenSpaceId: tea.String(fmt.Sprintf("dtv1.card//im_robot.%s", userId)),
		OutTrackId:  tea.String(outTrackId),
		UserId:      tea.String(userId),
		UserIdType:  tea.Int32(1),
	}

	headers := &dingtalkcard.CreateAndDeliverHeaders{
		XAcsDingtalkAccessToken: tea.String(accessToken),
	}

	_, err = s.card.CreateAndDeliverWithOptions(req, headers, &util.RuntimeOptions{})
	if err != nil {
		return "", err
	}
	return outTrackId, nil
}

// CreateAndDeliverGroup 投放「群聊」卡片，返回 outTrackId
func (s *CardSender) CreateAndDeliverGroup(ctx context.Context, userId, conversationId, initContent string) (string, error) {
	accessToken, err := s.loadAccessToken(ctx)
	if err != nil {
		return "", err
	}

	outTrackId := uuid.New().String()

	cardParamMap := map[string]*string{
		"content": tea.String(initContent),
	}

	req := &dingtalkcard.CreateAndDeliverRequest{
		CallbackType: tea.String("STREAM"),
		CardData: &dingtalkcard.CreateAndDeliverRequestCardData{
			CardParamMap: cardParamMap,
		},
		CardTemplateId: tea.String(s.templateId),
		ImGroupOpenDeliverModel: &dingtalkcard.CreateAndDeliverRequestImGroupOpenDeliverModel{
			RobotCode: tea.String(s.clientId),
			// 卡片接收人
			Recipients: []*string{tea.String(userId)},
		},
		ImGroupOpenSpaceModel: &dingtalkcard.CreateAndDeliverRequestImGroupOpenSpaceModel{
			SupportForward: tea.Bool(true),
		},
		OpenSpaceId: tea.String(fmt.Sprintf("dtv1.card//im_group.%s", conversationId)),
		OutTrackId:  tea.String(outTrackId),
		UserId:      tea.String(userId),
		UserIdType:  tea.Int32(1),
	}

	headers := &dingtalkcard.CreateAndDeliverHeaders{
		XAcsDingtalkAccessToken: tea.String(accessToken),
	}

	_, err = s.card.CreateAndDeliverWithOptions(req, headers, &util.RuntimeOptions{})
	if err != nil {
		return "", err
	}
	return outTrackId, nil
}

// StreamingUpdate 流式更新卡片内容（全量覆盖）
func (s *CardSender) StreamingUpdate(ctx context.Context, outTrackId, content string, finished bool) {
	accessToken, err := s.loadAccessToken(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "[dingtalk card] load access_token failed", slog.String("outTrackId", outTrackId), slog.String("error", err.Error()))
		return
	}
	request := &dingtalkcard.StreamingUpdateRequest{
		Content:    tea.String(content),
		Guid:       tea.String(uuid.New().String()),
		IsError:    tea.Bool(false),
		IsFinalize: tea.Bool(finished),
		IsFull:     tea.Bool(true),
		Key:        tea.String("content"),
		OutTrackId: tea.String(outTrackId),
	}

	headers := &dingtalkcard.StreamingUpdateHeaders{
		XAcsDingtalkAccessToken: tea.String(accessToken),
	}

	_, err = s.card.StreamingUpdateWithOptions(request, headers, &util.RuntimeOptions{})
	if err != nil {
		slog.ErrorContext(ctx, "[dingtalk card] update stream failed", slog.String("outTrackId", outTrackId), slog.String("error", err.Error()))
	}
}

func (s *CardSender) loadAccessToken(ctx context.Context) (string, error) {
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

	resp, err := helper.RestyClient.R().
		SetContext(ctx).
		SetBody(helper.X{
			"appKey":    s.clientId,
			"appSecret": s.clientSecret,
		}).
		Post("https://api.dingtalk.com/v1.0/oauth2/accessToken")
	if err != nil {
		slog.ErrorContext(ctx, "[dingtalk card] refresh access_token failed", slog.String("clientId", s.clientId), slog.String("clientSecret", s.clientSecret), slog.String("error", err.Error()))
		return
	}

	slog.InfoContext(ctx, "[dingtalk card] refresh access_token", slog.String("clientId", s.clientId), slog.String("clientSecret", s.clientSecret), slog.String("response", resp.String()))

	if !resp.IsSuccess() {
		slog.ErrorContext(ctx, "[dingtalk card] refresh access_token failed", slog.String("clientId", s.clientId), slog.String("clientSecret", s.clientSecret), slog.String("error", resp.Status()))
		return
	}

	ret := gjson.ParseBytes(resp.Body())
	at := AccessToken{
		Token:     ret.Get("accessToken").String(),
		ExpiredAt: time.Now().Unix() + ret.Get("expireIn").Int(),
	}
	b, _ := json.Marshal(at)
	if err := s.reduc.Set(ctx, s.tokenKey, string(b), 0).Err(); err != nil {
		slog.ErrorContext(ctx, "[dingtalk card] redis set access_token failed", slog.String("key", s.tokenKey), slog.String("value", string(b)), slog.String("error", err.Error()))
	}
}

func NewCardSender(clientId, clientSecret, cardTemplateId string, uc redis.UniversalClient) (*CardSender, error) {
	client, err := dingtalkcard.NewClient(&openapi.Config{
		Protocol: tea.String("https"),
		RegionId: tea.String("central"),
	})
	if err != nil {
		return nil, err
	}

	s := &CardSender{
		clientId:     clientId,
		clientSecret: clientSecret,
		templateId:   cardTemplateId,

		lockKey:  fmt.Sprintf("mutex:dingtalk:refresh_access_token:%s", clientId),
		tokenKey: fmt.Sprintf("assistant:dingtalk:access_token:%s", clientId),

		card:  client,
		reduc: uc,
	}

	go func() {
		ctx := context.Background()
		s.refreshAccessToken(ctx)
		for range time.Tick(time.Minute) {
			s.refreshAccessToken(ctx)
		}
	}()

	return s, nil
}
