package dingtalk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/noble-gase/ar/llmchat"
	"github.com/noble-gase/ne/helper"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/client"
)

type Bot struct {
	chat   *llmchat.Chat
	card   *CardSender
	client *client.StreamClient
}

func (b *Bot) Start() {
	b.client.RegisterChatBotCallbackRouter(b.messageHandler)
	if err := b.client.Start(context.Background()); err != nil {
		panic(fmt.Errorf("Dingtalk Start: %w", err))
	}
}

func (b *Bot) Stop() {
	fmt.Println("Stop ADK dingtalk bot ...")
	b.client.Close()
}

func (b *Bot) messageHandler(ctx context.Context, data *chatbot.BotCallbackDataModel) ([]byte, error) {
	ctx = helper.CtxWithTraceId(ctx)

	slog.InfoContext(ctx, "dingtalk message", slog.Any("data", data))

	var (
		outTrackId string
		err        error
	)
	if data.ConversationType == "2" { // 群聊
		outTrackId, err = b.card.CreateAndDeliverGroup(ctx, data.SenderStaffId, data.ConversationId, "> 思考中...")
	} else { // 单聊
		outTrackId, err = b.card.CreateAndDeliverRobot(ctx, data.SenderStaffId, "> 思考中...")
	}
	if err != nil {
		_ = b.reply(ctx, data.SessionWebhook, "抱歉，处理时出错了："+err.Error())
		return nil, nil
	}

	// 异步处理，让回调快速返回，避免钉钉超时重试
	go b.streamAnswer(context.WithoutCancel(ctx), outTrackId, data.SenderStaffId, data.Text.Content)

	return nil, nil
}

func (b *Bot) streamAnswer(ctx context.Context, outTrackId, userId, question string) {
	// 调用 adk-go Agent
	events, err := b.chat.Ask(ctx, userId, question)
	if err != nil {
		b.card.StreamingUpdate(ctx, outTrackId, "获取会话失败："+err.Error(), true)
		return
	}

	var accumulated strings.Builder
	for event, err := range events {
		if err != nil {
			b.card.StreamingUpdate(ctx, outTrackId, accumulated.String()+"\n\n> ⚠️ 出现错误："+err.Error(), true)
			return
		}

		// 最终 event：直接用它的完整内容，不再累积
		if event.IsFinalResponse() {
			if event.Content != nil {
				var final strings.Builder
				for _, part := range event.Content.Parts {
					if !part.Thought {
						final.WriteString(part.Text)
					}
				}
				b.card.StreamingUpdate(ctx, outTrackId, final.String(), true)
			}
			return
		}

		if event.Content != nil {
			for _, part := range event.Content.Parts {
				if !part.Thought {
					accumulated.WriteString(part.Text)
				}
			}
			b.card.StreamingUpdate(ctx, outTrackId, accumulated.String(), false)
		}
	}
}

func (b *Bot) reply(ctx context.Context, webhook, answer string) error {
	body := helper.X{
		"msgtype": "markdown",
		"markdown": helper.X{
			"title": b.chat.Name(),
			"text":  answer,
		},
	}
	resp, err := helper.RestyClient.R().
		SetContext(ctx).
		SetHeader("Accept", "application/json").
		SetHeader("Content-Type", "application/json").
		SetBody(body).
		Post(webhook)
	if err != nil {
		return err
	}
	if !resp.IsSuccess() {
		return errors.New(resp.String())
	}
	return nil
}

func NewBot(clientId, clientSecret string, chat *llmchat.Chat, card *CardSender) *Bot {
	cred := client.NewAppCredentialConfig(clientId, clientSecret)

	client := client.NewStreamClient(
		client.WithAppCredential(cred),
		client.WithAutoReconnect(true),
		client.WithKeepAlive(time.Minute),
	)

	return &Bot{
		chat:   chat,
		card:   card,
		client: client,
	}
}
