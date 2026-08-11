package dingtalk

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/noble-gase/argon/llmchat"
	"github.com/noble-gase/neon/helper"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/client"
	"google.golang.org/adk/v2/session"
)

// defaultRequestTimeout 限制单次运行（LLM + 工具调用）的总时长。异步 handler
// 通过 context.WithoutCancel 与回调解绑，所以需要它兜底，避免某次运行永远跑不完
// 而泄漏 goroutine。
const defaultRequestTimeout = time.Hour

// cardSettleTimeout 限制终态卡片更新的耗时，它跑在独立 context 上。
const cardSettleTimeout = 10 * time.Second

// defaultShutdownGrace 是停机时等待在途消息自然跑完的时长。它刻意远小于
// defaultRequestTimeout：后者是防泄漏的极限值，拿来当停机宽限会拖死发布。
const defaultShutdownGrace = 15 * time.Second

// defaultCancelKeywords 是放弃图工作流待答问题的关键词。
var defaultCancelKeywords = []string{"/cancel", "取消"}

// msgMeta 携带投放后续卡片（含人工确认卡片）所需的消息上下文。
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
	shutdownGrace  time.Duration
	confirm        *ConfirmCard
	cancelKeywords []string

	lifecycleMu sync.Mutex
	stopping    bool
	stopCtx     context.Context
	stopCancel  context.CancelFunc
	stopOnce    sync.Once
	inFlight    sync.WaitGroup
}

// ErrStopped 表示 Bot 已经停止，由 Start 返回。Bot 是一次性的：要继续服务请
// 新建一个。
var ErrStopped = errors.New("dingtalk: bot already stopped")

// Start 开始接收消息。Bot 是一次性的：Stop 之后不能重新 Start，要继续服务请新建一个。
func (b *Bot) Start() error {
	b.lifecycleMu.Lock()
	stopping := b.stopping
	b.lifecycleMu.Unlock()
	if stopping {
		return ErrStopped
	}

	b.client.RegisterChatBotCallbackRouter(b.messageHandler)
	b.client.RegisterCardCallbackRouter(b.confirmCardHandler)
	if err := b.client.Start(context.Background()); err != nil {
		return fmt.Errorf("start DingTalk bot: %w", err)
	}
	return nil
}

// Stop 停掉接收并等在途消息退出。
//
// 有上限的只是「自然排空」那一段，Stop 本身不保证有界：发出取消后它会一直等到
// 所有 handler 真正退出。Go 杀不死 goroutine，若提前返回并 Close 掉 card，残留任务就会
// 写已关闭的资源。完全忽略 context 的任务只能交给进程管理器的 shutdown deadline 收尾。
func (b *Bot) Stop() {
	b.stopOnce.Do(func() {
		slog.Info("[dingtalk bot] stopping")

		b.lifecycleMu.Lock()
		b.stopping = true
		b.lifecycleMu.Unlock()

		b.client.Close()

		// 先给在途消息把话说完：中途取消会在会话里留下半截事件，用户下次
		// 回来看到的就是一个停在半途的回答。
		if !b.drain(b.shutdownGrace) {
			slog.Warn("[dingtalk bot] drain timed out, cancelling in-flight work")
		}
		b.stopCancel()
		b.inFlight.Wait()
		b.card.Close()
	})
}

// drain 等待在途 handler 结束，返回它们是否都在 d 之内跑完。
func (b *Bot) drain(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.inFlight.Wait()
	}()

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// launch 启动与钉钉短生命周期回调 context 解绑的后台任务，但它仍受 Stop 的
// 取消与等待管辖。
func (b *Bot) launch(ctx context.Context, where string, fn func(context.Context)) bool {
	b.lifecycleMu.Lock()
	defer b.lifecycleMu.Unlock()
	if b.stopping {
		return false
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopForward := context.AfterFunc(b.stopCtx, cancel)
	b.inFlight.Go(func() {
		defer cancel()
		defer stopForward()
		// 兼底：这里是唯一的异步入口，handler 自己的 recover 只能盖住拿到
		// outTrackId 之后的部分，在那之前 panic 会直接打挂进程。
		defer func() {
			if r := recover(); r != nil {
				slog.ErrorContext(runCtx, "[dingtalk bot] panic", slog.String("where", where), slog.Any("error", r), slog.String("stack", string(debug.Stack())))
			}
		}()
		fn(runCtx)
	})
	return true
}

// recover 兜住异步 handler 里的 panic，记录带堆栈的日志，并把进行中的卡片收尾，
// 免得用户一直盯着「思考中...」。卡片更新用一个全新的 context，因为原来的可能
// 已经被取消或超时了。
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
	if !b.launch(ctx, "streamAnswer", func(runCtx context.Context) {
		b.streamAnswer(runCtx, meta, data.Text.Content, outTrackId)
	}) {
		b.settle(ctx, outTrackId, "> ⚠️ 服务正在停止，请稍后重试。")
	}

	return nil, nil
}

// locked 在持有用户锁的前提下执行 fn。
//
// 所有会驱动 ADK session 或改动确认记录的操作都必须在这里面完成：聊天消息、
// 确认回调、取消，三者共用同一把锁，因此彼此天然互斥，不需要第二套并发控制。
// 传给 fn 的 ctx 会在锁失去所有权时被取消，据此中止执行，避免与新持锁者并发写。
// 抢锁等待有独立于 b.timeout 的上限，超时返回 errUserBusy，调用方按「正忙」
// 而不是故障来呈现。
func (b *Bot) locked(ctx context.Context, userId string, fn func(ctx context.Context)) error {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	held, unlock, err := b.card.lockUser(ctx, userId)
	if err != nil {
		// 正常排队（用户连发两条）不是故障，按 ERROR 记会把真正的锁异常淹掉
		if errors.Is(err, errUserBusy) {
			slog.InfoContext(ctx, "[dingtalk bot] user busy, message not processed", slog.String("userId", userId))
		} else {
			slog.ErrorContext(ctx, "[dingtalk bot] lock user failed", slog.String("error", err.Error()), slog.String("userId", userId))
		}
		return err
	}
	defer unlock()

	fn(held)
	return nil
}

func (b *Bot) streamAnswer(ctx context.Context, meta msgMeta, text, outTrackId string) {
	defer b.recover(ctx, "streamAnswer", outTrackId)

	err := b.locked(ctx, meta.userId, func(ctx context.Context) {
		// 会话是待处理事项的唯一来源，渠道侧不维护副本
		pending, err := b.pending(ctx, meta.userId)
		if err != nil {
			// 状态不明时绝不能猜：当成新提问会把回答送错入口，当成回答又可能对不上 interrupt
			b.settle(ctx, outTrackId, "> ⚠️ 暂时无法处理，请稍后重试。")
			return
		}

		if len(pending.Inputs)+len(pending.Confirmations) != 0 && b.isCancel(text) {
			b.cancelPending(ctx, meta, outTrackId)
			return
		}

		// 确认卡才是这次流程的入口，普通消息不能在它之上另起一轮
		if len(pending.Confirmations) != 0 {
			b.settle(ctx, outTrackId, "> ⏳ 还有一次工具执行等待确认，请先在确认卡片上点击「同意」或「拒绝」，或回复「取消」放弃这次执行。")
			return
		}

		var (
			sessionId string
			seq       iter.Seq2[*session.Event, error]
		)
		if len(pending.Inputs) == 0 {
			sessionId, seq, err = b.chat.Ask(ctx, meta.userId, text)
		} else {
			next := pending.Inputs[0]
			sessionId, seq, err = b.chat.Reply(ctx, meta.userId, next.InterruptId, llmchat.ReplyPayload(text, next.ResponseSchema))
		}
		if err != nil {
			b.settle(ctx, outTrackId, "> ⚠️ 出现错误："+err.Error())
			return
		}
		b.handleAnswer(ctx, seq, meta, outTrackId, sessionId)
	})
	// 「正忙」是正常排队，「拿不到锁」是基础设施故障，不能用同一句话误导用户
	if errors.Is(err, errUserBusy) {
		b.settle(ctx, outTrackId, "> ⏳ 上一条消息还在处理中，请等它完成后再发。")
	} else if err != nil {
		b.settle(ctx, outTrackId, "> ⚠️ 暂时无法处理，请稍后重试。")
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
	confirmTrackIds, err := b.card.clearConfirms(ctx, meta.userId)
	for _, trackId := range confirmTrackIds {
		b.settle(ctx, trackId, cancelledConfirmText)
	}
	if err != nil {
		slog.ErrorContext(ctx, "[dingtalk bot] clear confirmations failed", slog.String("error", err.Error()), slog.String("userId", meta.userId))
		b.settle(ctx, outTrackId, "> 已开始新的对话。部分旧确认卡片可能没能变灰，点了也不会生效。")
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

// maxAutoRejects 限制一轮消息里连续自动拒绝的次数。确认入口一直建不起来时，
// 模型可能被拒绝后又请求一次确认，不能让它无限打转。
const maxAutoRejects = 3

// handleAnswer 消费一次运行的事件流，返回本轮是否失败。sessionId 是这次运行所在
// 的会话，需要随确认记录一起落库，以便跨日后能识别出旧卡片。
func (b *Bot) handleAnswer(ctx context.Context, seq iter.Seq2[*session.Event, error], meta msgMeta, outTrackId, sessionId string) (failed bool) {
	var result strings.Builder

	for range maxAutoRejects + 1 {
		confirm, note, failed := b.consume(ctx, seq, &result, meta, outTrackId, sessionId)
		if confirm == nil {
			return failed
		}

		if strings.TrimSpace(result.String()) != "" {
			result.WriteString("\n\n")
		}
		result.WriteString(note)

		resumed, err := b.chat.Confirm(ctx, meta.userId, sessionId, confirm.CallId, false, nil)
		if err != nil {
			// 撤不掉的确认会永远挂在会话上，把后续消息全部挡住，只能整个丢弃
			slog.ErrorContext(ctx, "[dingtalk bot] auto reject failed", slog.String("error", err.Error()), slog.String("userId", meta.userId), slog.String("callId", confirm.CallId))
			b.settle(ctx, outTrackId, result.String()+"\n\n"+b.discard(ctx, meta.userId))
			return true
		}
		seq = resumed
	}

	slog.ErrorContext(ctx, "[dingtalk bot] too many auto rejects", slog.String("userId", meta.userId))
	b.settle(ctx, outTrackId, result.String()+"\n\n"+b.discard(ctx, meta.userId))
	return true
}

// consume 消费一次运行的事件流，正文追加到 result。
//
// 返回非 nil 的确认表示没有可用的确认入口、需要自动拒绝后续跑，note 是给用户的
// 说明；返回 nil 表示本轮已经收尾，failed 即为结果。
func (b *Bot) consume(ctx context.Context, seq iter.Seq2[*session.Event, error], result *strings.Builder, meta msgMeta, outTrackId, sessionId string) (*llmchat.Confirmation, string, bool) {
	for event, err := range seq {
		if err != nil {
			// 出错也要走收尾：图很可能仍在等待某个问题，直接显示错误会让用户
			// 以为流程结束，下一条消息就被当成了回答
			b.finish(ctx, meta, outTrackId, result.String()+"\n\n> ⚠️ "+errorNote(err))
			return nil, "", true
		}

		// 工具需要人工确认：投放确认卡片并暂停，等待用户点击
		if confirm, ok := llmchat.ConfirmationOf(event); ok {
			// 没有可用的确认入口时必须自动拒绝，不能只显示提示：否则 ADK 仍停在
			// 确认态，而没有任何卡片能回答它，下一条消息只会被挡在门外。
			if b.confirm == nil {
				return confirm, fmt.Sprintf("> ⚠️ 工具「%s」需要人工确认，但未配置确认卡片，已自动拒绝。", confirm.ToolName), false
			}
			if err := b.requestConfirmation(ctx, meta, outTrackId, result.String(), sessionId, confirm); err != nil {
				return confirm, fmt.Sprintf("> ⚠️ 工具「%s」的确认卡片没能发出，已自动拒绝，请重新发起。", confirm.ToolName), false
			}
			// 流程已转交给新投放的确认卡片，本轮到此为止
			return nil, "", false
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
	return nil, "", false
}

// discard 丢弃整个自动会话，用于会话里留下了谁都回答不了的确认时。返回给用户的说明。
func (b *Bot) discard(ctx context.Context, userId string) string {
	if err := b.chat.ResetAutomatic(ctx, userId); err != nil {
		slog.ErrorContext(ctx, "[dingtalk bot] discard conversation failed", slog.String("error", err.Error()), slog.String("userId", userId))
		return "> ⚠️ 当前对话已无法继续，请回复「取消」重新开始。"
	}
	if _, err := b.card.clearConfirms(ctx, userId); err != nil {
		slog.ErrorContext(ctx, "[dingtalk bot] clear confirmations failed", slog.String("error", err.Error()), slog.String("userId", userId))
	}
	return "> 已开始新的对话，请重新发起。"
}

// pending 查询会话里仍在等待的问题与工具确认，失败时统一记日志，由调用方决定如何呈现。
func (b *Bot) pending(ctx context.Context, userId string) (*llmchat.Pending, error) {
	pending, err := b.chat.Pending(ctx, userId)
	if err != nil {
		slog.ErrorContext(ctx, "[dingtalk bot] load pending work failed", slog.String("error", err.Error()), slog.String("userId", userId))
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
	if len(pending.Inputs) != 0 {
		b.settle(ctx, outTrackId, inputNote(text, pending.Inputs[0], len(pending.Inputs)))
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

	// ConfirmCard 配置人工确认卡片：用哪个模板，以及按钮如何上报用户的决定。
	// 不需要确认时可以为 nil；为 nil 时不会配置任何决定解析规则，按钮点击也就
	// 无法被识别。
	ConfirmCard *ConfirmCard

	// Timeout 限制单次运行（LLM + 工具调用）的总时长，用于兜底防止 goroutine
	// 泄漏。为零时使用 defaultRequestTimeout（1 小时）。
	Timeout time.Duration

	// LockWait 是排队消息等待上一条消息释放用户锁的时长上限。它应远小于
	// Timeout：超过这个时长就提示用户「上一条消息还在处理中」，而不是陪着
	// 上一条消息跑完全程。为零时使用 defaultLockWait（30 秒）。
	LockWait time.Duration

	// ShutdownGrace 是 Stop 在发出取消之前，留给在途消息自己跑完的时长。它并不
	// 限制 Stop 本身：被取消的 handler 仍会等到它真正退出，因为提前返回会让它们
	// 写到已关闭的卡片客户端上。进程管理器的停机期限要配得比它长。
	// 为零时使用 defaultShutdownGrace（15 秒）。
	ShutdownGrace time.Duration

	// CancelKeywords 是放弃图工作流待答问题的关键词，免得用户在回答完之前一直被
	// 困住。匹配时对消息做 trim 且大小写不敏感。为 nil 时使用
	// defaultCancelKeywords。
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

	shutdownGrace := defaultShutdownGrace
	if cfg.ShutdownGrace > 0 {
		shutdownGrace = cfg.ShutdownGrace
	}

	cancelKeywords := defaultCancelKeywords
	if cfg.CancelKeywords != nil {
		cancelKeywords = cfg.CancelKeywords
	}

	stopCtx, stopCancel := context.WithCancel(context.Background())
	return &Bot{
		chat:           chat,
		card:           card,
		client:         client,
		timeout:        timeout,
		shutdownGrace:  shutdownGrace,
		confirm:        cfg.ConfirmCard,
		cancelKeywords: cancelKeywords,
		stopCtx:        stopCtx,
		stopCancel:     stopCancel,
	}
}
