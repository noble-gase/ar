package dingtalk

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"github.com/noble-gase/argon/llmchat"
	"github.com/noble-gase/neon/helper"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/client"
	"google.golang.org/adk/v2/session"
)

// defaultRequestTimeout bounds the total duration of a single run (LLM +
// tool calls). It is a safety net against leaked goroutines when a run never
// completes, because the async handlers are detached via context.WithoutCancel.
const defaultRequestTimeout = time.Hour

// msgMeta carries the message context needed to deliver follow-up cards,
// including Human-in-the-Loop confirmation cards.
type msgMeta struct {
	userId      string // 发送人 staffId
	convType    string // "2" 群聊，其余为单聊
	groupConvId string // 群聊的钉钉 conversationId
}

type Bot struct {
	chat    *llmchat.Chat
	card    *CardSender
	client  *client.StreamClient
	timeout time.Duration
	confirm *ConfirmCard
}

func (b *Bot) Start() {
	b.client.RegisterChatBotCallbackRouter(b.messageHandler)
	b.client.RegisterCardCallbackRouter(b.confirmCardHandler)
	if err := b.client.Start(context.Background()); err != nil {
		panic(fmt.Errorf("Dingtalk Start: %w", err))
	}
}

func (b *Bot) Stop() {
	fmt.Println("Stop ADK dingtalk bot ...")

	b.client.Close()
	b.card.Close()
}

// recover recovers from a panic in an async handler, logs it with a
// stack trace, and finalizes the in-progress card so the user is not left
// staring at "思考中..." forever. The card update uses a fresh context because
// the original one may already be cancelled or timed out.
func (b *Bot) recover(ctx context.Context, where, outTrackId string) {
	if r := recover(); r != nil {
		slog.ErrorContext(ctx, "[dingtalk bot] panic", slog.String("where", where), slog.Any("error", r), slog.String("stack", string(debug.Stack())))

		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		b.card.StreamingUpdate(uctx, outTrackId, "> ⚠️ 执行过程中出现内部错误，请稍后重试", true)
	}
}

func (b *Bot) messageHandler(ctx context.Context, data *chatbot.BotCallbackDataModel) ([]byte, error) {
	ctx = helper.CtxWithTraceId(ctx)

	slog.InfoContext(ctx, "[dingtalk bot] chat message", slog.Any("data", data))

	var (
		outTrackId string
		err        error
	)
	if data.ConversationType == "2" { // 群聊
		outTrackId, err = b.card.CreateAndDeliverGroup(ctx, data.SenderStaffId, data.ConversationId)
	} else { // 单聊
		outTrackId, err = b.card.CreateAndDeliverRobot(ctx, data.SenderStaffId)
	}
	if err != nil {
		slog.ErrorContext(ctx, "[dingtalk bot] card create failed", slog.String("error", err.Error()))
		return nil, err
	}

	meta := msgMeta{
		userId:      data.SenderStaffId,
		convType:    data.ConversationType,
		groupConvId: data.ConversationId,
	}

	// 异步处理，让回调快速返回，避免钉钉超时重试
	go b.streamAnswer(context.WithoutCancel(ctx), meta, data.Text.Content, outTrackId)

	return nil, nil
}

func (b *Bot) streamAnswer(ctx context.Context, meta msgMeta, text, outTrackId string) {
	defer b.recover(ctx, "streamAnswer", outTrackId)

	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	seq, err := b.chat.Ask(ctx, meta.userId, text)
	if err != nil {
		b.card.StreamingUpdate(ctx, outTrackId, "> ⚠️ 出现错误："+err.Error(), true)
		return
	}
	b.handleAnswer(ctx, seq, meta, outTrackId)
}

func (b *Bot) handleAnswer(ctx context.Context, seq iter.Seq2[*session.Event, error], meta msgMeta, outTrackId string) {
	var result strings.Builder

	// event处理
	for event, err := range seq {
		if err != nil {
			b.card.StreamingUpdate(ctx, outTrackId, result.String()+"\n\n> ⚠️ 出现错误："+err.Error(), true)
			return
		}

		// 工具需要人工确认：投放确认卡片并暂停，等待用户点击
		if confirm, ok := llmchat.ConfirmationOf(event); ok {
			// 未配置确认卡片：无法发起确认，直接在普通卡片上输出提示并结束，
			// 不暂停工具执行流程。
			if b.confirm == nil {
				note := result.String()
				if strings.TrimSpace(note) != "" {
					note += "\n\n"
				}
				note += fmt.Sprintf("> ⚠️ 工具「%s」需要人工确认，但未配置确认卡片，无法发起确认，已跳过执行。", confirm.ToolName)
				b.card.StreamingUpdate(ctx, outTrackId, note, true)
				return
			}
			b.requestConfirmation(ctx, meta, outTrackId, result.String(), confirm)
			return
		}

		// 非最终event 或 内容为空，则跳过
		if !event.IsFinalResponse() || event.Content == nil {
			continue
		}

		for _, part := range event.Content.Parts {
			if !part.Thought {
				result.WriteString(part.Text)
			}
		}
	}

	// 更新卡片内容
	b.card.StreamingUpdate(ctx, outTrackId, result.String(), true)
}

type Config struct {
	ClientId       string
	ClientSecret   string
	CardTemplateId string

	// ConfirmCard configures the Human-in-the-Loop confirmation card: which
	// template to use and how its buttons report the user's decision. It may be
	// nil when confirmation is not needed; when nil, no decision parsing rules
	// are configured (button clicks won't be recognized unless a ConfirmCard is
	// provided).
	ConfirmCard *ConfirmCard

	// Timeout bounds the total duration of a single run (LLM + tool calls).
	// It is a safety net against leaked goroutines. If zero,
	// defaultRequestTimeout (1h) is used.
	Timeout time.Duration
}

func NewBot(cfg *Config, chat *llmchat.Chat, card *CardSender) *Bot {
	cred := client.NewAppCredentialConfig(cfg.ClientId, cfg.ClientSecret)

	client := client.NewStreamClient(
		client.WithAppCredential(cred),
		client.WithAutoReconnect(true),
		client.WithKeepAlive(time.Minute),
	)

	timeout := defaultRequestTimeout
	if cfg.Timeout > 0 {
		timeout = cfg.Timeout
	}

	return &Bot{
		chat:    chat,
		card:    card,
		client:  client,
		timeout: timeout,
		confirm: cfg.ConfirmCard,
	}
}
