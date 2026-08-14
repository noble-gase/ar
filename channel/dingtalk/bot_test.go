package dingtalk

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/noble-gase/argon/llmchat"
	"github.com/noble-gase/argon/userlock"
	adk_session "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

func confirmationEvent(callId, toolName string) *adk_session.Event {
	ev := &adk_session.Event{}
	ev.Content = &genai.Content{
		Role: string(genai.RoleModel),
		Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{
				ID:   callId,
				Name: toolconfirmation.FunctionCallName,
				Args: map[string]any{"originalFunctionCall": map[string]any{"name": toolName}},
			},
		}},
	}
	return ev
}

func textEvent(text string) *adk_session.Event {
	ev := &adk_session.Event{}
	ev.Content = genai.NewContentFromText(text, genai.RoleModel)
	return ev
}

var meta = msgMeta{userId: "u1"}

func ask(id, message string, schema *jsonschema.Schema) *llmchat.RequestInput {
	return &llmchat.RequestInput{InterruptId: id, Message: message, ResponseSchema: schema}
}

// 任何流错误都要复查待回答状态：图很可能仍在等待，直接报错会让用户以为流程结束。
func TestStreamErrorStillPromptsPending(t *testing.T) {
	card := newFakeCard()
	chat := &fakeChat{
		pending:   []*llmchat.RequestInput{ask("a", "问题 A", nil)},
		streamErr: errRedisDown,
	}
	b := newTestBot(card, chat)

	b.streamAnswer(context.Background(), meta, "答案", "track")

	last := card.lastCard()
	if !strings.Contains(last, "出现错误") {
		t.Errorf("card = %q, want the error surfaced", last)
	}
	if !strings.Contains(last, "问题 A") {
		t.Errorf("card = %q, want the still-pending question re-asked", last)
	}
}

// 锁丢失后 ctx 已取消，但终态卡片仍必须写出去，否则用户看不到任何反馈。
func TestCardStillRendersAfterLockLost(t *testing.T) {
	card, chat := newFakeCard(), &fakeChat{}
	card.loseLock = true
	b := newTestBot(card, chat)

	_ = b.locked(context.Background(), meta.userId, func(ctx context.Context) {
		b.settle(ctx, "track", "> ⚠️ 出现错误：lost lock")
	})

	if !strings.Contains(card.lastCard(), "出现错误") {
		t.Errorf("card = %q, want the error rendered even though the context was cancelled", card.lastCard())
	}
}

// 锁被别人接手后必须立刻中止，不能继续驱动同一个会话。
func TestLostLockAbortsRun(t *testing.T) {
	card, chat := newFakeCard(), &fakeChat{}
	card.loseLock = true
	b := newTestBot(card, chat)

	_ = b.locked(context.Background(), meta.userId, func(ctx context.Context) {
		if ctx.Err() != nil {
			return
		}
		t.Error("work ran with a lost lock")
	})

	if chat.askedText != "" {
		t.Errorf("askedText = %q, want no session activity after losing the lock", chat.askedText)
	}
}

// 查询待回答状态失败时不能显示成完成态：图可能仍在等待，用户会以为结束了而发出
// 新提问，那条消息又会被当成回答。
func TestFinishDoesNotFakeCompletionWhenStateUnknown(t *testing.T) {
	card, chat := newFakeCard(), &fakeChat{pendingErr: errRedisDown}
	b := newTestBot(card, chat)

	b.finish(context.Background(), meta, "track", "已处理")

	last := card.lastCard()
	if !strings.Contains(last, "已处理") {
		t.Errorf("card = %q, want the result kept", last)
	}
	if !strings.Contains(last, "无法确认") {
		t.Errorf("card = %q, want the unknown state surfaced instead of a clean finish", last)
	}
}

// 确认回调与聊天消息驱动同一个 session，必须共用用户锁。
func TestConfirmResumeSerializesWithMessages(t *testing.T) {
	card := newFakeCard()
	card.savePending(context.Background(), "track-1", &pendingConfirm{CallId: "call-1", UserId: "u1"})
	chat := &concurrencyChat{}
	b := newTestBot(card, chat)
	b.confirm = testConfirmCard()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		b.streamAnswer(context.Background(), meta, "在吗", "track")
	}()
	go func() {
		defer wg.Done()
		b.resumeConfirmed(context.Background(), meta, "track-1", true)
	}()
	wg.Wait()

	if got := chat.maxConcurrent.Load(); got != 1 {
		t.Errorf("max concurrent turns = %d, want 1 (confirm callback must take the user lock)", got)
	}
	if got := chat.calls.Load(); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
}

// 没有待回答问题时，消息就是一次普通提问。
func TestMessageWithoutPendingIsAQuestion(t *testing.T) {
	card, chat := newFakeCard(), &fakeChat{events: []*adk_session.Event{textEvent("你好")}}
	b := newTestBot(card, chat)

	b.streamAnswer(context.Background(), meta, "在吗", "track")

	if chat.askedText != "在吗" {
		t.Errorf("askedText = %q, want the message to reach the model", chat.askedText)
	}
	if chat.repliedId != "" {
		t.Errorf("repliedId = %q, want no reply", chat.repliedId)
	}
	if card.lastCard() != "你好" {
		t.Errorf("card = %q, want the answer", card.lastCard())
	}
}

// 有待回答问题时，消息是回答，且必须送给最早未答的那一个。
func TestMessageAnswersOldestPending(t *testing.T) {
	card := newFakeCard()
	chat := &fakeChat{pending: []*llmchat.RequestInput{
		ask("b", "问题 B", nil),
		ask("a2", "问题 A2", nil),
	}}
	b := newTestBot(card, chat)

	b.streamAnswer(context.Background(), meta, "答案", "track")

	if chat.repliedId != "b" {
		t.Errorf("repliedId = %q, want the oldest pending %q", chat.repliedId, "b")
	}
	if chat.askedText != "" {
		t.Errorf("askedText = %q, want the answer NOT to be sent as a new question", chat.askedText)
	}
	// 收尾时展示的下一题必须就是下一条消息会回答的那一题
	if !strings.Contains(card.lastCard(), "问题 A2") {
		t.Errorf("card = %q, want it to prompt the next pending question", card.lastCard())
	}
}

// 节点要求结构化数据时，文本回答要按 schema 解析后再送回。
func TestStructuredAnswerIsDecoded(t *testing.T) {
	card := newFakeCard()
	chat := &fakeChat{pending: []*llmchat.RequestInput{
		ask("a", "是否批准", &jsonschema.Schema{Type: "object"}),
	}}
	b := newTestBot(card, chat)

	b.streamAnswer(context.Background(), meta, `{"approved":true}`, "track")

	got, ok := chat.repliedArg.(map[string]any)
	if !ok || got["approved"] != true {
		t.Errorf("repliedArg = %#v, want a decoded object", chat.repliedArg)
	}
}

// 回答不合 schema：节点仍在等待，所以同一个问题会被再问一次，且不暴露原始报错。
func TestRejectedAnswerIsAskedAgain(t *testing.T) {
	card := newFakeCard()
	chat := &fakeChat{
		pending:   []*llmchat.RequestInput{ask("a", "是否批准", &jsonschema.Schema{Type: "object"})},
		streamErr: workflow.ErrInvalidResumeResponse,
	}
	b := newTestBot(card, chat)

	b.streamAnswer(context.Background(), meta, "随便说说", "track")

	last := card.lastCard()
	if !strings.Contains(last, "回答格式不符合要求") {
		t.Errorf("card = %q, want a friendly retry notice", last)
	}
	if !strings.Contains(last, "是否批准") {
		t.Errorf("card = %q, want the same question asked again", last)
	}
	if strings.Contains(last, "resume response") {
		t.Errorf("card = %q, want the raw ADK error hidden", last)
	}
	if !strings.Contains(last, "JSON") {
		t.Errorf("card = %q, want the JSON requirement repeated", last)
	}
}

// 其他错误不会吞掉待回答问题：它仍在会话里，收尾时会被重新问出来。
func TestPendingSurvivesStreamError(t *testing.T) {
	card := newFakeCard()
	chat := &fakeChat{
		pending:   []*llmchat.RequestInput{ask("a", "问题 A", nil)},
		streamErr: errRedisDown,
	}
	b := newTestBot(card, chat)

	b.streamAnswer(context.Background(), meta, "答案", "track")

	got, _ := chat.Pending(context.Background(), "u1")
	if len(got.Inputs) != 1 || got.Inputs[0].InterruptId != "a" {
		t.Errorf("pending = %v, want the question preserved", got.Inputs)
	}
}

func TestMissingConfirmCardAutoRejects(t *testing.T) {
	card := newFakeCard()
	chat := &fakeChat{
		confirmed:        make(chan confirmCall, 1),
		sessionId:        "s-today",
		useConfirmEvents: true,
	}
	ev := &adk_session.Event{}
	ev.Content = &genai.Content{
		Role: string(genai.RoleModel),
		Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{
				Name: toolconfirmation.FunctionCallName,
				ID:   "call-1",
			},
		}},
	}
	chat.events = []*adk_session.Event{ev}
	b := newTestBot(card, chat) // ConfirmCard 故意留空

	b.streamAnswer(context.Background(), meta, "开始", "track")

	got := awaitConfirm(t, chat)
	if got.callId != "call-1" || got.approved {
		t.Errorf("Confirm(%q, %v), want (call-1, false)", got.callId, got.approved)
	}
	if !strings.Contains(card.lastCard(), "已自动拒绝") {
		t.Errorf("card = %q, want the missing confirmation UI explained", card.lastCard())
	}
}

// 扇出让多个节点同时暂停时，展示第一个并提示剩余数量。
func TestFanOutPausesArePrompted(t *testing.T) {
	card := newFakeCard()
	chat := &fakeChat{pending: []*llmchat.RequestInput{
		ask("a", "问题 A", nil),
		ask("b", "问题 B", nil),
	}}
	// 本轮没有回答，只是发起提问
	chat.pending = append([]*llmchat.RequestInput(nil), chat.pending...)
	b := newTestBot(card, chat)

	b.finish(context.Background(), meta, "track", "处理中")

	last := card.lastCard()
	if !strings.Contains(last, "问题 A") {
		t.Errorf("card = %q, want the first question shown", last)
	}
	if !strings.Contains(last, "还有 1 个问题待回答") {
		t.Errorf("card = %q, want the remaining count", last)
	}
	if !strings.Contains(last, "处理中") {
		t.Errorf("card = %q, want the prior text kept", last)
	}
}

// 会话状态读不到时绝不能猜：既不能当新提问，也不能当回答。
func TestUnknownPendingStateIsNotGuessed(t *testing.T) {
	card, chat := newFakeCard(), &fakeChat{pendingErr: errRedisDown}
	b := newTestBot(card, chat)

	b.streamAnswer(context.Background(), meta, "一些内容", "track")

	if chat.askedText != "" || chat.repliedId != "" {
		t.Errorf("askedText=%q repliedId=%q, want no guess", chat.askedText, chat.repliedId)
	}
	if !strings.Contains(card.lastCard(), "稍后重试") {
		t.Errorf("card = %q, want the user asked to retry", card.lastCard())
	}
}

// 无待回答问题时，取消词只是一句普通消息。
func TestCancelWithoutPendingGoesToModel(t *testing.T) {
	card, chat := newFakeCard(), &fakeChat{}
	b := newTestBot(card, chat)

	b.streamAnswer(context.Background(), meta, "取消", "track")

	if chat.askedText != "取消" {
		t.Errorf("askedText = %q, want the message to reach the model", chat.askedText)
	}
	if chat.resetCalls != 0 {
		t.Errorf("resetCalls = %d, want 0", chat.resetCalls)
	}
}

// 确认卡投不出去时，ADK 里的确认必须被自动拒绝掉：否则会话上挂着一个谁都回答不了
// 的确认，Redis 里又没有记录，下一条消息会绕过所有闸门直接开新 run。
func TestFailedConfirmCardDoesNotStrandTheSession(t *testing.T) {
	card, chat := newFakeCard(), &fakeChat{events: []*adk_session.Event{confirmationEvent("call-1", "danger_tool")}}
	card.confirmDeliverErr = errRedisDown
	b := newTestBot(card, chat)
	b.confirm = testConfirmCard()

	b.streamAnswer(context.Background(), meta, "删掉它", "track")

	if card.pendingCount() != 0 {
		t.Errorf("pendingCount = %d, want the undelivered record cleaned up", card.pendingCount())
	}
	if got := chat.lastConfirm(); got.callId != "call-1" || got.approved {
		t.Fatalf("confirm = %+v, want call-1 auto rejected", got)
	}

	// 第二条消息：确认已经撤掉了，这条才该走到模型
	chat.events = nil
	b.streamAnswer(context.Background(), meta, "你好", "track2")

	if chat.askedText != "你好" {
		t.Errorf("askedText = %q, want the next message to run normally", chat.askedText)
	}
}

// discard 清掉的确认卡必须写成「已随对话取消」，不能仍显示可点的同意/拒绝。
func TestDiscardSettlesConfirmCards(t *testing.T) {
	card := newFakeCard()
	if err := card.savePending(context.Background(), "confirm-track", &pendingConfirm{CallId: "call-1", UserId: "u1"}); err != nil {
		t.Fatalf("savePending() error = %v", err)
	}
	b := newTestBot(card, &fakeChat{})

	note := b.discard(context.Background(), "u1")
	if !strings.Contains(note, "已开始新的对话") {
		t.Errorf("discard() = %q, want a new-conversation notice", note)
	}
	if card.pendingCount() != 0 {
		t.Errorf("pendingCount = %d, want confirmations cleared", card.pendingCount())
	}
	if !strings.Contains(card.lastCard(), "已随对话取消") {
		t.Errorf("card = %q, want the leftover confirm card marked cancelled", card.lastCard())
	}
}

// 待答问题和待确认调用来自同一次会话加载：全历史查询在长会话里不便宜，路由判断
// 不该为此读两遍。收尾时要看运行之后的最新状态，那次是必须的。
func TestPendingLoadedOncePerMessage(t *testing.T) {
	card, chat := newFakeCard(), &fakeChat{}
	b := newTestBot(card, chat)

	b.streamAnswer(context.Background(), meta, "你好", "track")

	if got := chat.loads(); got != 2 {
		t.Errorf("session loads = %d, want 2: one to route the message, one to finish", got)
	}
}

// 工具确认不在待答问题列表里。挂着未决确认时再发普通消息，不能另起一轮——
// 那会让确认卡指向的执行和新 run 同时挂在一个会话上。
//
// 判定依据是 ADK 会话：这里刻意不写 Redis 记录，模拟记录丢失或被淘汰。
func TestMessageBlockedWhileConfirmationPending(t *testing.T) {
	card := newFakeCard()
	chat := &fakeChat{confirms: []*llmchat.Confirmation{{CallId: "call-1", ToolName: "danger_tool"}}}
	b := newTestBot(card, chat)

	b.streamAnswer(context.Background(), meta, "再帮我查个别的", "track")

	if chat.askedText != "" {
		t.Errorf("askedText = %q, want no new run while a confirmation is pending", chat.askedText)
	}
	if !strings.Contains(card.lastCard(), "等待确认") {
		t.Errorf("card = %q, want the user pointed at the confirmation card", card.lastCard())
	}
}

// 反过来：Redis 里留着已经作废的旧记录时，不能把用户挡在门外——会话里没有未决
// 确认，这条消息就该正常跑。
func TestStaleConfirmRecordDoesNotBlockMessages(t *testing.T) {
	card, chat := newFakeCard(), &fakeChat{}
	if err := card.savePending(context.Background(), "confirm-track", &pendingConfirm{CallId: "gone", UserId: "u1"}); err != nil {
		t.Fatalf("savePending() error = %v", err)
	}
	b := newTestBot(card, chat)

	b.streamAnswer(context.Background(), meta, "你好", "track")

	if chat.askedText != "你好" {
		t.Errorf("askedText = %q, want a stale channel-side record not to block the user", chat.askedText)
	}
}

// 只有工具确认、没有图问题时，「取消」同样要生效。
func TestCancelWorksWithOnlyConfirmationPending(t *testing.T) {
	card := newFakeCard()
	chat := &fakeChat{confirms: []*llmchat.Confirmation{{CallId: "call-1", ToolName: "danger_tool"}}}
	if err := card.savePending(context.Background(), "confirm-track", &pendingConfirm{CallId: "call-1", UserId: "u1"}); err != nil {
		t.Fatalf("savePending() error = %v", err)
	}
	b := newTestBot(card, chat)

	b.streamAnswer(context.Background(), meta, "取消", "track")

	if chat.resetCalls != 1 {
		t.Errorf("resetCalls = %d, want the conversation reset", chat.resetCalls)
	}
	if card.pendingCount() != 0 {
		t.Errorf("pendingCount = %d, want the confirmation invalidated", card.pendingCount())
	}
	if chat.askedText != "" {
		t.Errorf("askedText = %q, want the cancel keyword not to reach the model", chat.askedText)
	}
}

// 查不到确认记录时状态不明，不能赌「没有未决确认」就直接开跑。
func TestUnknownConfirmationStateStopsProcessing(t *testing.T) {
	card, chat := newFakeCard(), &fakeChat{}
	chat.confirmsErr = errRedisDown
	b := newTestBot(card, chat)

	b.streamAnswer(context.Background(), meta, "你好", "track")

	if chat.askedText != "" {
		t.Errorf("askedText = %q, want no run when the confirmation state is unknown", chat.askedText)
	}
	if !strings.Contains(card.lastCard(), "暂时无法处理") {
		t.Errorf("card = %q, want the user asked to retry", card.lastCard())
	}
}

// 取消要真正丢弃 ADK 中 waiting 的图状态，而不只是不再追问。
func TestCancelResetsSession(t *testing.T) {
	card := newFakeCard()
	if err := card.savePending(context.Background(), "confirm-track", &pendingConfirm{CallId: "call-1", UserId: "u1"}); err != nil {
		t.Fatalf("savePending() error = %v", err)
	}
	chat := &fakeChat{pending: []*llmchat.RequestInput{ask("a", "问题 A", nil)}}
	b := newTestBot(card, chat)

	b.streamAnswer(context.Background(), meta, "/cancel", "track")

	if chat.resetCalls != 1 {
		t.Errorf("resetCalls = %d, want 1", chat.resetCalls)
	}
	if chat.askedText != "" {
		t.Errorf("askedText = %q, want the cancel keyword not to reach the model", chat.askedText)
	}
	if got, _ := chat.Pending(context.Background(), "u1"); len(got.Inputs) != 0 {
		t.Errorf("pending = %v, want empty after cancel", got)
	}
	card.mu.Lock()
	defer card.mu.Unlock()
	var cancelled bool
	for _, content := range card.cards {
		cancelled = cancelled || strings.Contains(content, "已随对话取消")
	}
	if !cancelled {
		t.Errorf("cards = %v, want the old confirmation card invalidated visibly", card.cards)
	}
}

func TestCancelReportsFailure(t *testing.T) {
	card := newFakeCard()
	chat := &fakeChat{
		pending:  []*llmchat.RequestInput{ask("a", "问题 A", nil)},
		resetErr: errRedisDown,
	}
	b := newTestBot(card, chat)

	b.streamAnswer(context.Background(), meta, "/cancel", "track")

	if !strings.Contains(card.lastCard(), "取消失败") {
		t.Errorf("card = %q, want a failure notice", card.lastCard())
	}
}

func TestCancelSettlesKnownCardsWhenRedisCleanupFails(t *testing.T) {
	card := newFakeCard()
	if err := card.savePending(context.Background(), "confirm-track", &pendingConfirm{CallId: "call-1", UserId: "u1"}); err != nil {
		t.Fatalf("savePending() error = %v", err)
	}
	card.clearConfirmsErr = errRedisDown
	chat := &fakeChat{pending: []*llmchat.RequestInput{ask("a", "问题 A", nil)}}
	b := newTestBot(card, chat)

	b.streamAnswer(context.Background(), meta, "/cancel", "track")

	card.mu.Lock()
	defer card.mu.Unlock()
	var cancelled, warned bool
	for _, content := range card.cards {
		cancelled = cancelled || strings.Contains(content, "已随对话取消")
		warned = warned || strings.Contains(content, "没能变灰")
	}
	if !cancelled || !warned {
		t.Errorf("cards = %v, want known old cards settled and cleanup failure reported", card.cards)
	}
}

// 拿不到用户锁时不能继续，否则就是并发驱动同一个会话。
func TestLockFailureStopsProcessing(t *testing.T) {
	card, chat := newFakeCard(), &fakeChat{}
	card.lockErr = errRedisDown
	b := newTestBot(card, chat)

	b.streamAnswer(context.Background(), meta, "在吗", "track")

	if chat.askedText != "" {
		t.Errorf("askedText = %q, want processing to stop without the lock", chat.askedText)
	}
	if !strings.Contains(card.lastCard(), "稍后重试") {
		t.Errorf("card = %q, want the user asked to retry", card.lastCard())
	}
}

// 用户忙是正常排队，要如实告知「上一条还在处理」，不能按基础设施故障处理。
func TestLockBusyShowsBusyMessage(t *testing.T) {
	card, chat := newFakeCard(), &fakeChat{}
	card.lockErr = userlock.ErrBusy
	b := newTestBot(card, chat)

	b.streamAnswer(context.Background(), meta, "在吗", "track")

	if chat.askedText != "" {
		t.Errorf("askedText = %q, want processing to stop while busy", chat.askedText)
	}
	if !strings.Contains(card.lastCard(), "还在处理中") {
		t.Errorf("card = %q, want a busy notice", card.lastCard())
	}
}

// 同一用户的并发消息必须串行进入 ADK。
func TestSameUserMessagesAreSerialized(t *testing.T) {
	card := newFakeCard()
	chat := &concurrencyChat{}
	b := newTestBot(card, chat)

	const senders = 8
	var wg sync.WaitGroup
	wg.Add(senders)
	for i := range senders {
		go func() {
			defer wg.Done()
			b.streamAnswer(context.Background(), meta, fmt.Sprintf("msg-%d", i), "track")
		}()
	}
	wg.Wait()

	if got := chat.maxConcurrent.Load(); got != 1 {
		t.Errorf("max concurrent turns = %d, want 1", got)
	}
	if got := chat.calls.Load(); got != senders {
		t.Errorf("calls = %d, want %d", got, senders)
	}
}

// 不同用户之间不应互相阻塞。
func TestDifferentUsersRunInParallel(t *testing.T) {
	card := newFakeCard()
	release := make(chan struct{})
	chat := &concurrencyChat{hold: release}
	b := newTestBot(card, chat)

	var wg sync.WaitGroup
	wg.Add(2)
	for _, user := range []string{"u1", "u2"} {
		go func() {
			defer wg.Done()
			b.streamAnswer(context.Background(), msgMeta{userId: user}, "hi", "track")
		}()
	}

	deadline := time.After(2 * time.Second)
	for chat.inFlight.Load() < 2 {
		select {
		case <-deadline:
			close(release)
			wg.Wait()
			t.Fatalf("only %d user(s) ran concurrently, want 2", chat.inFlight.Load())
		default:
		}
	}
	close(release)
	wg.Wait()
}

func TestSchemaHint(t *testing.T) {
	if got := schemaHint(nil); got != "" {
		t.Errorf("schemaHint(nil) = %q, want empty", got)
	}
	if got := schemaHint(&jsonschema.Schema{Type: "string"}); got != "" {
		t.Errorf("schemaHint(string) = %q, want empty", got)
	}

	got := schemaHint(&jsonschema.Schema{Type: "object"})
	if !strings.Contains(got, "JSON") || !strings.Contains(got, "object") {
		t.Errorf("schemaHint(object) = %q, want a JSON hint mentioning the schema", got)
	}
}

func TestIsCancel(t *testing.T) {
	b := &Bot{cancelKeywords: defaultCancelKeywords}

	for _, text := range []string{"/cancel", "取消", "  /CANCEL  "} {
		if !b.isCancel(text) {
			t.Errorf("isCancel(%q) = false, want true", text)
		}
	}
	for _, text := range []string{"", "cancel the order", "取消订单"} {
		if b.isCancel(text) {
			t.Errorf("isCancel(%q) = true, want false", text)
		}
	}
}

func TestInputNote(t *testing.T) {
	tests := []struct {
		name      string
		prior     string
		input     *llmchat.RequestInput
		remaining int
		want      []string
		notWant   []string
	}{
		{
			name:      "single question keeps prior text",
			prior:     "初稿已生成",
			input:     ask("a", "是否发布？", nil),
			remaining: 1,
			want:      []string{"初稿已生成", "> ❓ 是否发布？", "请直接回复消息继续。"},
			notWant:   []string{"待回答"},
		},
		{
			name:      "multiple questions hint the remainder",
			input:     ask("a", "预算是多少？", nil),
			remaining: 3,
			want:      []string{"> ❓ 预算是多少？", "还有 2 个问题待回答"},
		},
		{
			name:      "blank message falls back",
			input:     ask("a", "   ", nil),
			remaining: 1,
			want:      []string{"> ❓ 请补充信息"},
		},
		{
			name:      "payload is rendered",
			input:     &llmchat.RequestInput{InterruptId: "a", Message: "确认参数", Payload: map[string]any{"id": "x"}},
			remaining: 1,
			want:      []string{"附加信息：", `"id"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inputNote(tt.prior, tt.input, tt.remaining)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("inputNote() = %q, want it to contain %q", got, want)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("inputNote() = %q, want it NOT to contain %q", got, notWant)
				}
			}
		})
	}
}
