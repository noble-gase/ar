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

// cardSettleTimeout 限制终态卡片更新的耗时，它跑在独立 context 上。
const cardSettleTimeout = 10 * time.Second

// defaultCancelKeywords are the messages that drop the questions a graph
// workflow is waiting on.
var defaultCancelKeywords = []string{"/cancel", "取消"}

// msgMeta carries the message context needed to deliver follow-up cards,
// including Human-in-the-Loop confirmation cards.
type msgMeta struct {
	userId      string // 发送人 staffId
	convType    string // "2" 群聊，其余为单聊
	groupConvId string // 群聊的钉钉 conversationId
}

type Bot struct {
	chat           chatClient
	card           cardStore
	client         *client.StreamClient
	timeout        time.Duration
	confirm        *ConfirmCard
	cancelKeywords []string
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
		b.settle(ctx, outTrackId, "> ⚠️ 执行过程中出现内部错误，请稍后重试")
	}
}

// deliverCard 按会话类型投放卡片并返回 outTrackId。
func (b *Bot) deliverCard(ctx context.Context, meta msgMeta) (string, error) {
	if meta.convType == "2" { // 群聊
		return b.card.CreateAndDeliverGroup(ctx, meta.userId, meta.groupConvId)
	}
	return b.card.CreateAndDeliverRobot(ctx, meta.userId)
}

// settle 写入卡片的终态。用独立的 context：调用方的 ctx 可能因超时或
// 丢失用户锁而已取消，那样用户就看不到错误或待回答提示了。
func (b *Bot) settle(ctx context.Context, outTrackId, content string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cardSettleTimeout)
	defer cancel()

	b.card.StreamingUpdate(ctx, outTrackId, content, true)
}

func (b *Bot) messageHandler(ctx context.Context, data *chatbot.BotCallbackDataModel) ([]byte, error) {
	ctx = helper.CtxWithTraceId(ctx)

	slog.InfoContext(ctx, "[dingtalk bot] chat message", slog.Any("data", data))

	meta := msgMeta{
		userId:      data.SenderStaffId,
		convType:    data.ConversationType,
		groupConvId: data.ConversationId,
	}

	outTrackId, err := b.deliverCard(ctx, meta)
	if err != nil {
		slog.ErrorContext(ctx, "[dingtalk bot] card create failed", slog.String("error", err.Error()))
		return nil, err
	}

	// 异步处理，让回调快速返回，避免钉钉超时重试
	go b.streamAnswer(context.WithoutCancel(ctx), meta, data.Text.Content, outTrackId)

	return nil, nil
}

// outcome 是一次会话驱动的结果。
type outcome int

const (
	// outcomeDone 本轮跑完，没有遗留的待处理入口。
	outcomeDone outcome = iota

	// outcomeAwaiting 流程已转交给新投放的确认卡片。
	outcomeAwaiting

	// outcomeFailed 本轮失败。
	outcomeFailed
)

// locked 在持有用户锁的前提下执行 fn。
//
// 所有会驱动 ADK session 或改动确认记录的操作都必须在这里面完成：聊天消息、
// 确认回调、取消，三者共用同一把锁，因此彼此天然互斥，不需要第二套并发控制。
// 传给 fn 的 ctx 会在锁失去所有权时被取消，据此中止执行，避免与新持锁者并发写。
func (b *Bot) locked(ctx context.Context, userId string, fn func(ctx context.Context)) error {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	held, unlock, err := b.card.lockUser(ctx, userId)
	if err != nil {
		slog.ErrorContext(ctx, "[dingtalk bot] lock user failed", slog.String("error", err.Error()), slog.String("userId", userId))
		return err
	}
	defer unlock()

	fn(held)
	return nil
}

func (b *Bot) streamAnswer(ctx context.Context, meta msgMeta, text, outTrackId string) {
	defer b.recover(ctx, "streamAnswer", outTrackId)

	err := b.locked(ctx, meta.userId, func(ctx context.Context) {
		// 会话是待回答问题的唯一来源，渠道侧不维护副本
		pending, err := b.pending(ctx, meta.userId)
		if err != nil {
			// 状态不明时绝不能猜：当成新提问会把回答送错入口，当成回答又可能对不上 interrupt
			b.settle(ctx, outTrackId, "> ⚠️ 暂时无法处理，请稍后重试。")
			return
		}

		if len(pending) != 0 && b.isCancel(text) {
			b.cancelPending(ctx, meta, outTrackId)
			return
		}

		var (
			sessionId string
			seq       iter.Seq2[*session.Event, error]
		)
		if len(pending) == 0 {
			sessionId, seq, err = b.chat.Ask(ctx, meta.userId, text)
		} else {
			next := pending[0]
			sessionId, seq, err = b.chat.Reply(ctx, meta.userId, next.InterruptId, llmchat.ReplyPayload(text, next.ResponseSchema))
		}
		if err != nil {
			b.settle(ctx, outTrackId, "> ⚠️ 出现错误："+err.Error())
			return
		}
		b.handleAnswer(ctx, seq, meta, outTrackId, sessionId)
	})
	if err != nil {
		b.settle(ctx, outTrackId, "> ⚠️ 上一条消息还在处理中，请稍后重试。")
	}
}

// cancelPending 重置自动会话，丢弃 ADK 中 waiting 的图状态。
func (b *Bot) cancelPending(ctx context.Context, meta msgMeta, outTrackId string) {
	if err := b.chat.ResetAutomatic(ctx, meta.userId); err != nil {
		slog.ErrorContext(ctx, "[dingtalk bot] reset automatic conversation failed", slog.String("error", err.Error()), slog.String("userId", meta.userId))
		b.settle(ctx, outTrackId, "> ⚠️ 取消失败，请稍后重试。")
		return
	}

	// 会话都没了，旧卡片上的按钮不能再去恢复一个已不存在的工具调用
	if err := b.card.clearConfirms(ctx, meta.userId); err != nil {
		slog.ErrorContext(ctx, "[dingtalk bot] clear confirmations failed", slog.String("error", err.Error()), slog.String("userId", meta.userId))
		b.settle(ctx, outTrackId, "> ⚠️ 已开始新的对话，但旧的确认卡片可能仍然可点，请勿再点击。")
		return
	}
	b.settle(ctx, outTrackId, "> 已取消待回答的问题，并开始新的对话。")
}

// isCancel 判断消息是否为放弃回答的关键词。
func (b *Bot) isCancel(text string) bool {
	text = strings.TrimSpace(text)
	for _, kw := range b.cancelKeywords {
		if strings.EqualFold(text, kw) {
			return true
		}
	}
	return false
}

// handleAnswer 消费一次运行的事件流。sessionId 是这次运行所在的会话，需要随确认
// 记录一起落库，以便跨日后能识别出旧卡片。
func (b *Bot) handleAnswer(ctx context.Context, seq iter.Seq2[*session.Event, error], meta msgMeta, outTrackId, sessionId string) outcome {
	var result strings.Builder

	for event, err := range seq {
		if err != nil {
			// 出错也要走收尾：图很可能仍在等待某个问题，直接显示错误会让用户
			// 以为流程结束，下一条消息就被当成了回答
			b.finish(ctx, meta, outTrackId, result.String()+"\n\n> ⚠️ "+errorNote(err))
			return outcomeFailed
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
				b.settle(ctx, outTrackId, note)
				return outcomeDone
			}
			// 子确认没能建立起来时必须向上报失败，否则父确认会被当成已完成，
			// 而新的确认入口其实并不存在
			if err := b.requestConfirmation(ctx, meta, outTrackId, result.String(), sessionId, confirm); err != nil {
				// 确认入口没建起来，但图里可能还有待回答的问题，仍要收尾展示
				b.finish(ctx, meta, outTrackId, result.String()+"\n\n> ⚠️ 无法发起人工确认，请重新发起。")
				return outcomeFailed
			}
			return outcomeAwaiting
		}

		// 图节点的提问由收尾时统一从会话读取，这里只是避免把它当成正文
		if _, ok := llmchat.RequestInputOf(event); ok {
			continue
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

	b.finish(ctx, meta, outTrackId, result.String())
	return outcomeDone
}

// pending 查询会话里仍未回答的问题，失败时统一记日志，由调用方决定如何呈现。
func (b *Bot) pending(ctx context.Context, userId string) ([]*llmchat.RequestInput, error) {
	pending, err := b.chat.PendingInputs(ctx, userId)
	if err != nil {
		slog.ErrorContext(ctx, "[dingtalk bot] load pending inputs failed", slog.String("error", err.Error()), slog.String("userId", userId))
	}
	return pending, err
}

// finish 收尾：图若仍在等待人工输入就展示下一个问题，否则输出本轮结果。
func (b *Bot) finish(ctx context.Context, meta msgMeta, outTrackId, text string) {
	pending, err := b.pending(ctx, meta.userId)
	if err != nil {
		// 不能显示成完成态：图可能仍在等待，用户会以为结束了而发出新提问，
		// 那条消息又会被当成回答
		b.settle(ctx, outTrackId, text+"\n\n> ⚠️ 暂时无法确认流程是否结束，请稍后再发一条消息确认。")
		return
	}
	if len(pending) != 0 {
		b.settle(ctx, outTrackId, inputNote(text, pending[0], len(pending)))
		return
	}
	b.settle(ctx, outTrackId, text)
}

// errorNote 把事件流里的错误转成给用户看的文案。回答不合 schema 是可预期的，
// 不该把 ADK 的原始报错抛给用户。
func errorNote(err error) string {
	if llmchat.IsRejectedReply(err) {
		return "回答格式不符合要求，请重新回答。"
	}
	return "出现错误：" + err.Error()
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

	// CancelKeywords are the messages that abandon the questions a graph workflow
	// is waiting on, so the user is not stuck answering until the queue expires.
	// Matched case-insensitively against the trimmed message. If nil,
	// defaultCancelKeywords is used.
	CancelKeywords []string
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

	cancelKeywords := defaultCancelKeywords
	if cfg.CancelKeywords != nil {
		cancelKeywords = cfg.CancelKeywords
	}

	return &Bot{
		chat:           chat,
		card:           card,
		client:         client,
		timeout:        timeout,
		confirm:        cfg.ConfirmCard,
		cancelKeywords: cancelKeywords,
	}
}
