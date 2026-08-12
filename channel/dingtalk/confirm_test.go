package dingtalk

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/noble-gase/argon/llmchat"
	"github.com/noble-gase/argon/userlock"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/card"
	adk_session "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

func testConfirmCard() *ConfirmCard {
	return &ConfirmCard{
		ParamKey: "action",
		Approve:  ConfirmAction{Value: "approve"},
		Reject:   ConfirmAction{Value: "reject"},
	}
}

func cardRequest(outTrackId, userId string, params map[string]any, actionIds ...string) *card.CardRequest {
	req := &card.CardRequest{OutTrackId: outTrackId, UserId: userId}
	req.CardActionData.CardPrivateData.Params = params
	req.CardActionData.CardPrivateData.ActionIdList = actionIds
	return req
}

func confirmBot(t *testing.T) (*Bot, *fakeCard, *fakeChat) {
	t.Helper()

	cardStore, chat := newFakeCard(), &fakeChat{confirmed: make(chan confirmCall, 1), sessionId: "s-today"}
	b := newTestBot(cardStore, chat)
	b.confirm = testConfirmCard()

	cardStore.savePending(context.Background(), "track-1", &pendingConfirm{
		CallId:    "call-1",
		UserId:    "u1",
		SessionId: "s-today",
	})
	return b, cardStore, chat
}

// awaitConfirm 等待后台 resumeConfirmed 真正调用 Confirm。
func awaitConfirm(t *testing.T, chat *fakeChat) confirmCall {
	t.Helper()

	select {
	case got := <-chat.confirmed:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("Confirm was never called")
		return confirmCall{}
	}
}

// waitFor 轮询等待条件成立：resumeConfirmed 的收尾发生在 Confirm 之后，
// 断言不能只等到 Confirm 被调用。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.After(2 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestConfirmApprovedResumesTool(t *testing.T) {
	b, cardStore, chat := confirmBot(t)

	resp, err := b.confirmCardHandler(context.Background(), cardRequest("track-1", "u1", map[string]any{"action": "approve"}))
	if err != nil {
		t.Fatalf("confirmCardHandler() error = %v", err)
	}

	got := awaitConfirm(t, chat)
	if got.callId != "call-1" || !got.approved {
		t.Errorf("Confirm(%q, %v), want (call-1, true)", got.callId, got.approved)
	}
	waitFor(t, "the confirmation to be completed", func() bool { return cardStore.pendingCount() == 0 })
	if !strings.Contains(resp.CardData.CardParamMap["content"], "已同意") {
		t.Errorf("card content = %q, want an approval notice", resp.CardData.CardParamMap["content"])
	}
}

func TestConfirmRejected(t *testing.T) {
	b, _, chat := confirmBot(t)

	resp, err := b.confirmCardHandler(context.Background(), cardRequest("track-1", "u1", map[string]any{"action": "reject"}))
	if err != nil {
		t.Fatalf("confirmCardHandler() error = %v", err)
	}

	if got := awaitConfirm(t, chat); got.approved {
		t.Error("Confirm() approved = true, want false")
	}
	if !strings.Contains(resp.CardData.CardParamMap["content"], "已拒绝") {
		t.Errorf("card content = %q, want a rejection notice", resp.CardData.CardParamMap["content"])
	}
}

// 决策也可以来自 actionId，用于模板不回传 params 的卡片。
func TestConfirmDecisionFromActionId(t *testing.T) {
	b, _, chat := confirmBot(t)

	if _, err := b.confirmCardHandler(context.Background(), cardRequest("track-1", "u1", nil, "approve")); err != nil {
		t.Fatalf("confirmCardHandler() error = %v", err)
	}
	if got := awaitConfirm(t, chat); !got.approved {
		t.Error("Confirm() approved = false, want true")
	}
}

// 他人不能替发起者做决定，且不能消费掉待确认状态。
func TestConfirmRejectsOtherUser(t *testing.T) {
	b, cardStore, chat := confirmBot(t)

	if _, err := b.confirmCardHandler(context.Background(), cardRequest("track-1", "intruder", map[string]any{"action": "approve"})); err != nil {
		t.Fatalf("confirmCardHandler() error = %v", err)
	}

	if cardStore.pendingCount() != 1 {
		t.Errorf("pendingCount = %d, want the confirmation left untouched", cardStore.pendingCount())
	}
	select {
	case got := <-chat.confirmed:
		t.Fatalf("Confirm was called by a non-owner: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
}

// 无法识别的决策不能消费待确认状态，否则用户再点就没反应了。
func TestConfirmIgnoresUnknownDecision(t *testing.T) {
	b, cardStore, _ := confirmBot(t)

	if _, err := b.confirmCardHandler(context.Background(), cardRequest("track-1", "u1", map[string]any{"action": "maybe"})); err != nil {
		t.Fatalf("confirmCardHandler() error = %v", err)
	}
	if cardStore.pendingCount() != 1 {
		t.Errorf("pendingCount = %d, want the confirmation still pending", cardStore.pendingCount())
	}
}

// 未配置 ConfirmCard 时无法解析按钮，必须保持待确认而不是误判为拒绝。
func TestConfirmWithoutCardConfigIsIgnored(t *testing.T) {
	b, cardStore, _ := confirmBot(t)
	b.confirm = nil

	if _, err := b.confirmCardHandler(context.Background(), cardRequest("track-1", "u1", map[string]any{"action": "approve"})); err != nil {
		t.Fatalf("confirmCardHandler() error = %v", err)
	}
	if cardStore.pendingCount() != 1 {
		t.Errorf("pendingCount = %d, want the confirmation still pending", cardStore.pendingCount())
	}
}

// 确认结束后迟到的重复回调：卡片上写的已经是这次确认的结果（「✅ 已同意」），
// 同步响应绝不能把它改写掉。
func TestLateClickDoesNotOverwriteFinalCard(t *testing.T) {
	b, _, _ := confirmBot(t)

	resp, err := b.confirmCardHandler(context.Background(), cardRequest("other-track", "u1", map[string]any{"action": "approve"}))
	if err != nil {
		t.Fatalf("confirmCardHandler() error = %v, want nil", err)
	}
	if resp == nil {
		t.Fatal("confirmCardHandler() response = nil, want an empty response")
	}
	if resp.CardData != nil {
		t.Errorf("card data = %v, want no card update for an already settled confirmation", resp.CardData)
	}
}

// 锁内读不到记录说明这次确认已在别处收尾（上一次点击写了终态，或取消流程写了
// 「已随对话取消」），此时绝不能再写任何文案覆盖它。
func TestResumeMissingPendingLeavesCardUntouched(t *testing.T) {
	cardStore, chat := newFakeCard(), &fakeChat{}
	b := newTestBot(cardStore, chat)

	b.resumeConfirmed(context.Background(), msgMeta{userId: "u1"}, "missing-track", true)

	if got := cardStore.lastCard(); got != "" {
		t.Errorf("card = %q, want no update: the final text was written elsewhere", got)
	}
	if cardStore.answerCards != 0 {
		t.Errorf("answerCards = %d, want no answer card for a missing confirmation", cardStore.answerCards)
	}
}

// 恢复过程中又触发一次确认、而子确认卡建不起来时：ADK 里已经产生了一个没有任何
// 卡片能回答的确认，必须自动拒绝掉，同时父记录要作废（事件流已消费，不能重放）。
func TestChildConfirmFailureAutoRejects(t *testing.T) {
	b, cardStore, chat := confirmBot(t)

	confirmEvent := &adk_session.Event{}
	confirmEvent.Content = &genai.Content{
		Role: string(genai.RoleModel),
		Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{ID: "child", Name: toolconfirmation.FunctionCallName},
		}},
	}
	chat.events = []*adk_session.Event{confirmEvent}
	// 子确认卡登记失败
	cardStore.saveErr = errRedisDown

	if _, err := b.confirmCardHandler(context.Background(), cardRequest("track-1", "u1", map[string]any{"action": "approve"})); err != nil {
		t.Fatalf("confirmCardHandler() error = %v", err)
	}
	awaitConfirm(t, chat)

	// 事件流已经开始消费，父 callId 已被 ADK 记录，绝不能重放
	waitFor(t, "the parent confirmation to be dropped", func() bool { return cardStore.pendingCount() == 0 })
}

// 自动会话按自然日轮换。跨日后旧卡片指向的那次执行已被放弃，再点只能明确告知过期，
// 不能去恢复一个已经不存在的调用，也不能邀请用户重试。
func TestExpiredConfirmationIsNotResumed(t *testing.T) {
	b, cardStore, chat := confirmBot(t)
	chat.sessionId = "s-today"
	cardStore.savePending(context.Background(), "track-1", &pendingConfirm{
		CallId:    "call-1",
		UserId:    "u1",
		SessionId: "s-yesterday",
	})

	b.resumeConfirmed(context.Background(), msgMeta{userId: "u1"}, "track-1", true)

	select {
	case got := <-chat.confirmed:
		t.Fatalf("Confirm resumed callId %q from an abandoned conversation", got.callId)
	default:
	}
	if cardStore.pendingCount() != 0 {
		t.Errorf("pendingCount = %d, want the expired record dropped", cardStore.pendingCount())
	}
	if !strings.Contains(cardStore.lastCard(), "已过期") {
		t.Errorf("card = %q, want the user told the confirmation expired", cardStore.lastCard())
	}
	if strings.Contains(cardStore.lastCard(), "请重新点击") {
		t.Errorf("card = %q, want no retry invitation: tapping again can never work", cardStore.lastCard())
	}
}

// 确认记录里的会话必须来自产生该确认的那次运行，否则跨过午夜的执行会把记录挂到
// 第二天的会话上，反而绕过过期校验。
func TestConfirmationRecordsRunSession(t *testing.T) {
	cardStore, chat := newFakeCard(), &fakeChat{sessionId: "s-run"}
	b := newTestBot(cardStore, chat)
	b.confirm = testConfirmCard()

	if err := b.requestConfirmation(context.Background(), msgMeta{userId: "u1"}, "track", "", "s-run", &llmchat.Confirmation{
		CallId:   "call-1",
		ToolName: "danger_tool",
	}); err != nil {
		t.Fatalf("requestConfirmation() error = %v", err)
	}

	saved := cardStore.onlyPending(t)
	if saved.SessionId != "s-run" {
		t.Errorf("SessionId = %q, want the conversation the run happened in", saved.SessionId)
	}
}

// 记录清理失败后用户再点一次：会话历史里已有这次决定，工具绝不能执行第二次。
func TestAlreadyConfirmedIsNotReExecuted(t *testing.T) {
	b, cardStore, chat := confirmBot(t)
	chat.confirmErr = llmchat.ErrAlreadyConfirmed

	b.resumeConfirmed(context.Background(), msgMeta{userId: "u1"}, "track-1", true)

	if cardStore.pendingCount() != 0 {
		t.Errorf("pendingCount = %d, want the stale record cleaned up", cardStore.pendingCount())
	}
	if !strings.Contains(cardStore.lastCard(), "已经处理过") {
		t.Errorf("card = %q, want the user told it was already handled", cardStore.lastCard())
	}
	if strings.Contains(cardStore.lastCard(), "请重新点击") {
		t.Errorf("card = %q, want no retry invitation for an already answered confirmation", cardStore.lastCard())
	}
}

func TestMissingConfirmationAfterSameDayResetExpiresCard(t *testing.T) {
	b, cardStore, chat := confirmBot(t)
	chat.confirmErr = llmchat.ErrConfirmationNotFound

	b.resumeConfirmed(context.Background(), msgMeta{userId: "u1"}, "track-1", true)

	if cardStore.pendingCount() != 0 {
		t.Errorf("pendingCount = %d, want the stale record removed", cardStore.pendingCount())
	}
	if !strings.Contains(cardStore.lastCard(), "已失效") {
		t.Errorf("card = %q, want the stale confirmation explained", cardStore.lastCard())
	}
}

// 回答卡还没建起来就 panic：此时只有确认卡需要收尾，但它必须被收尾，
// 否则会永远停在「处理中」。
func TestPanicBeforeAnswerCardSettlesConfirmCard(t *testing.T) {
	b, cardStore, _ := confirmBot(t)
	cardStore.panicOnDeliver = true

	b.resumeConfirmed(context.Background(), msgMeta{userId: "u1"}, "track-1", true)

	if !strings.Contains(cardStore.lastCard(), "请重新点击") {
		t.Errorf("card = %q, want the confirmation card settled after an early panic", cardStore.lastCard())
	}
	if cardStore.pendingCount() != 1 {
		t.Errorf("pendingCount = %d, want the record kept so the user can retry", cardStore.pendingCount())
	}
}

// panic 时两张卡都要收尾：只收回答卡的话，确认卡会永远停在「处理中」。
func TestPanicSettlesConfirmCard(t *testing.T) {
	b, cardStore, chat := confirmBot(t)
	chat.panicOnConfirm = true

	b.resumeConfirmed(context.Background(), msgMeta{userId: "u1"}, "track-1", true)

	if !strings.Contains(cardStore.lastCard(), "请重新点击") {
		t.Errorf("card = %q, want the confirmation card settled after a panic", cardStore.lastCard())
	}
}

// 恢复过程中 panic 时，确认记录必须保留，用户可以再点一次。
func TestConfirmPanicKeepsPending(t *testing.T) {
	b, cardStore, chat := confirmBot(t)
	chat.panicOnConfirm = true

	b.resumeConfirmed(context.Background(), msgMeta{userId: "u1"}, "track-1", true)

	if cardStore.pendingCount() != 1 {
		t.Errorf("pendingCount = %d, want the confirmation still actionable", cardStore.pendingCount())
	}
}

// 删除记录失败只影响清理，不能重放父 callId。
func TestDropFailureDoesNotReplay(t *testing.T) {
	b, cardStore, chat := confirmBot(t)
	cardStore.dropErr = errRedisDown

	if _, err := b.confirmCardHandler(context.Background(), cardRequest("track-1", "u1", map[string]any{"action": "approve"})); err != nil {
		t.Fatalf("confirmCardHandler() error = %v", err)
	}
	awaitConfirm(t, chat)

	// 删除失败也不能重投确认卡：父 callId 已被 ADK 消费
	waitFor(t, "the resume to finish", func() bool { return cardStore.deliveredConfirms() == 0 && cardStore.answerCards == 1 })
}

// 事件流一旦开始消费，ADK 就已记录这次决策，父 callId 不能再被重放：
// 中途失败只能作废记录并告知用户，绝不能重投确认卡让用户再点一次。
func TestStreamFailureDoesNotReplayParent(t *testing.T) {
	b, cardStore, chat := confirmBot(t)
	chat.streamErr = errRedisDown

	if _, err := b.confirmCardHandler(context.Background(), cardRequest("track-1", "u1", map[string]any{"action": "approve"})); err != nil {
		t.Fatalf("confirmCardHandler() error = %v", err)
	}
	awaitConfirm(t, chat)

	waitFor(t, "the confirmation to be dropped", func() bool { return cardStore.pendingCount() == 0 })
	if cardStore.deliveredConfirms() != 0 {
		t.Errorf("deliveredConfirms = %d, want no retry card: the callId was already consumed", cardStore.deliveredConfirms())
	}
}

// ADK 还没被调用就失败时，确认必须能重来：记录原样保留，原卡片提示用户再点一次。
// 按钮从不隐藏，所以不需要「另投一张确认卡」这条补偿链路。
func TestRetryOnOriginalCardWhenResumeNotStarted(t *testing.T) {
	b, cardStore, chat := confirmBot(t)
	cardStore.deliverErr = errRedisDown

	b.resumeConfirmed(context.Background(), msgMeta{userId: "u1"}, "track-1", true)

	if cardStore.pendingCount() != 1 {
		t.Errorf("pendingCount = %d, want the confirmation still actionable", cardStore.pendingCount())
	}
	if cardStore.deliveredConfirms() != 0 {
		t.Errorf("deliveredConfirms = %d, want no extra card: the original buttons still work", cardStore.deliveredConfirms())
	}
	if !strings.Contains(cardStore.lastCard(), "请重新点击") {
		t.Errorf("card = %q, want the original card to invite a retry", cardStore.lastCard())
	}
	select {
	case got := <-chat.confirmed:
		t.Fatalf("Confirm ran even though the answer card failed: %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

// 用户忙时确认同样原样保留，提示等上一条消息完成后重点，而不是按故障处理。
func TestConfirmBusyPromptsRetryAfterCurrentRun(t *testing.T) {
	b, cardStore, _ := confirmBot(t)
	cardStore.lockErr = userlock.ErrBusy

	b.resumeConfirmed(context.Background(), msgMeta{userId: "u1"}, "track-1", true)

	if cardStore.pendingCount() != 1 {
		t.Errorf("pendingCount = %d, want the confirmation preserved while busy", cardStore.pendingCount())
	}
	if !strings.Contains(cardStore.lastCard(), "还在处理中") {
		t.Errorf("card = %q, want a busy notice", cardStore.lastCard())
	}
}

// 拿不到用户锁时同样什么都没做，原卡片要能重试。
func TestRetryOnOriginalCardWhenLockUnavailable(t *testing.T) {
	b, cardStore, _ := confirmBot(t)
	cardStore.lockErr = errRedisDown

	b.resumeConfirmed(context.Background(), msgMeta{userId: "u1"}, "track-1", true)

	if cardStore.pendingCount() != 1 {
		t.Errorf("pendingCount = %d, want the confirmation preserved", cardStore.pendingCount())
	}
	if !strings.Contains(cardStore.lastCard(), "请重新点击") {
		t.Errorf("card = %q, want the original card to invite a retry", cardStore.lastCard())
	}
}

// 同步响应只把文案改成「处理中」：此刻后台还没开始恢复，不能显示成已完成。
func TestSyncResponseShowsProcessing(t *testing.T) {
	resp := confirmStatusResponse(processingText(true))
	content := resp.CardData.CardParamMap["content"]
	if !strings.Contains(content, "正在执行") {
		t.Errorf("content = %q, want a processing state", content)
	}
	if len(resp.CardData.CardParamMap) != 1 {
		t.Errorf("params = %v, want only the content: buttons must stay usable for retries", resp.CardData.CardParamMap)
	}
}

// 确认状态先落库再投卡；投卡失败时记录必须清理，不能留下点了没反应的入口。
func TestConfirmCardDeliveryFailureDropsPending(t *testing.T) {
	card := newFakeCard()
	card.confirmDeliverErr = errRedisDown
	chat := &fakeChat{}
	b := newTestBot(card, chat)
	b.confirm = testConfirmCard()

	err := b.requestConfirmation(context.Background(), msgMeta{userId: "u1"}, "track", "", "s-today", &llmchat.Confirmation{
		CallId:   "call-1",
		ToolName: "danger_tool",
	})
	if err == nil {
		t.Fatal("requestConfirmation() error = nil, want the delivery failure reported")
	}
	if card.pendingCount() != 0 {
		t.Errorf("pendingCount = %d, want no orphaned record when the card was never delivered", card.pendingCount())
	}
}

// 并发重复点击只能派发一次，不能多开一张空白回答卡片。
func TestConfirmDoubleClickDeliversOneAnswerCard(t *testing.T) {
	b, cardStore, chat := confirmBot(t)
	req := cardRequest("track-1", "u1", map[string]any{"action": "approve"})

	for range 3 {
		if _, err := b.confirmCardHandler(context.Background(), req); err != nil {
			t.Fatalf("confirmCardHandler() error = %v", err)
		}
	}
	awaitConfirm(t, chat)
	waitFor(t, "the confirmation to be completed", func() bool { return cardStore.pendingCount() == 0 })

	if cardStore.answerCards != 1 {
		t.Errorf("answerCards = %d, want exactly one answer card", cardStore.answerCards)
	}
	select {
	case got := <-chat.confirmed:
		t.Fatalf("Confirm ran twice: %+v", got)
	case <-time.After(50 * time.Millisecond):
	}

	// 后到的点击读不到记录时必须静默退出，不能用「已失效」覆盖决定文案
	if !b.drain(2 * time.Second) {
		t.Fatal("in-flight resumes did not finish")
	}
	if !strings.Contains(cardStore.lastCard(), "已同意") {
		t.Errorf("card = %q, want the decision text preserved after late clicks", cardStore.lastCard())
	}
}

// 会话被重置后，旧卡片上的按钮不能再去恢复一个已不存在的工具调用。
func TestCancelInvalidatesConfirmations(t *testing.T) {
	card := newFakeCard()
	card.savePending(context.Background(), "track-1", &pendingConfirm{CallId: "call-1", UserId: "u1"})
	chat := &fakeChat{pending: []*llmchat.RequestInput{{InterruptId: "a", Message: "问题 A"}}}
	b := newTestBot(card, chat)
	b.confirm = testConfirmCard()

	b.streamAnswer(context.Background(), msgMeta{userId: "u1"}, "/cancel", "track")

	if card.pendingCount() != 0 {
		t.Fatalf("pendingCount = %d, want confirmations invalidated with the session", card.pendingCount())
	}

	// 旧卡片再点也不应恢复任何东西
	resp, err := b.confirmCardHandler(context.Background(), cardRequest("track-1", "u1", map[string]any{"action": "approve"}))
	if err != nil {
		t.Fatalf("confirmCardHandler() error = %v", err)
	}
	if resp == nil {
		t.Fatal("confirmCardHandler() response = nil")
	}
	if card.answerCards != 0 {
		t.Errorf("answerCards = %d, want the stale click ignored", card.answerCards)
	}
}

// Redis 抖动必须让钉钉重试，否则用户的点击被静默丢弃。
func TestConfirmRedisFailureIsRetryable(t *testing.T) {
	b, cardStore, _ := confirmBot(t)
	cardStore.loadErr = errRedisDown

	if _, err := b.confirmCardHandler(context.Background(), cardRequest("track-1", "u1", map[string]any{"action": "approve"})); err == nil {
		t.Fatal("confirmCardHandler() error = nil, want an error so DingTalk retries")
	}
}

// 抢锁失败时什么都不做：记录原样保留，不会有任何 ADK 操作发生。
func TestConfirmKeepsPendingWhenLockFails(t *testing.T) {
	b, cardStore, chat := confirmBot(t)

	if _, err := b.confirmCardHandler(context.Background(), cardRequest("track-1", "u1", map[string]any{"action": "approve"})); err != nil {
		t.Fatalf("confirmCardHandler() error = %v", err)
	}
	awaitConfirm(t, chat)
	waitFor(t, "the confirmation to be completed", func() bool { return cardStore.pendingCount() == 0 })

	// 抢锁失败的场景：记录不能被消费
	b2, store2, chat2 := confirmBot(t)
	store2.lockErr = errRedisDown

	b2.resumeConfirmed(context.Background(), msgMeta{userId: "u1"}, "track-1", true)

	if store2.pendingCount() != 1 {
		t.Errorf("pendingCount = %d, want the confirmation preserved when the lock could not be taken", store2.pendingCount())
	}
	select {
	case got := <-chat2.confirmed:
		t.Fatalf("Confirm ran without the lock: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
}

// 并发点击只能恢复一次工具执行。
func TestConfirmDoubleClickResumesOnce(t *testing.T) {
	b, cardStore, chat := confirmBot(t)
	req := cardRequest("track-1", "u1", map[string]any{"action": "approve"})

	if _, err := b.confirmCardHandler(context.Background(), req); err != nil {
		t.Fatalf("confirmCardHandler() first error = %v", err)
	}
	awaitConfirm(t, chat)

	if _, err := b.confirmCardHandler(context.Background(), req); err != nil {
		t.Fatalf("confirmCardHandler() second error = %v", err)
	}
	select {
	case got := <-chat.confirmed:
		t.Fatalf("Confirm was called twice: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	if len(cardStore.dropped) != 1 {
		t.Errorf("consumed = %v, want exactly one", cardStore.dropped)
	}
}
