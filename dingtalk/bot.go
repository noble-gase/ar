package dingtalk

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"time"

	"github.com/noble-gase/ar/llmchat"
	"github.com/noble-gase/ne/helper"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/client"
	"google.golang.org/adk/session"
)

type EventHandler func(ctx context.Context, seq iter.Seq2[*session.Event, error], sender *CardSender, outTrackId string)

type Bot struct {
	chat    *llmchat.Chat
	card    *CardSender
	client  *client.StreamClient
	handler EventHandler
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
		slog.ErrorContext(ctx, "[dingtalk bot] card create failed", slog.String("error", err.Error()))
		_ = b.reply(ctx, data.SessionWebhook, "抱歉，处理时出错了："+err.Error())
		return nil, nil
	}

	// 异步处理，让回调快速返回，避免钉钉超时重试
	go b.streamAnswer(context.WithoutCancel(ctx), outTrackId, data.SenderStaffId, data.Text.Content)

	return nil, nil
}

func (b *Bot) streamAnswer(ctx context.Context, userId, text, outTrackId string) {
	seq, err := b.chat.Ask(ctx, userId, text)
	if err != nil {
		b.card.StreamingUpdate(ctx, outTrackId, "> ⚠️ 出现错误："+err.Error(), true)
		return
	}
	if b.handler != nil {
		b.handler(ctx, seq, b.card, outTrackId)
		return
	}
	b.defaultEventHandler(ctx, seq, outTrackId)
}

func (b *Bot) defaultEventHandler(ctx context.Context, seq iter.Seq2[*session.Event, error], outTrackId string) {
	var accumulated strings.Builder
	for event, err := range seq {
		if err != nil {
			b.card.StreamingUpdate(ctx, outTrackId, accumulated.String()+"\n\n> ⚠️ 出现错误："+err.Error(), true)
			return
		}

		// 最终event：直接用它的完整内容，不再累积
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

func (b *Bot) reply(ctx context.Context, webhook, text string) error {
	body := helper.X{
		"msgtype": "markdown",
		"markdown": helper.X{
			"title": b.chat.Name(),
			"text":  text,
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

type Config struct {
	ClientId       string
	ClientSecret   string
	CardTemplateId string
	EventHandler   EventHandler
}

func NewBot(cfg *Config, chat *llmchat.Chat, card *CardSender) *Bot {
	cred := client.NewAppCredentialConfig(cfg.ClientId, cfg.ClientSecret)

	client := client.NewStreamClient(
		client.WithAppCredential(cred),
		client.WithAutoReconnect(true),
		client.WithKeepAlive(time.Minute),
	)

	return &Bot{
		chat:    chat,
		card:    card,
		client:  client,
		handler: cfg.EventHandler,
	}
}
