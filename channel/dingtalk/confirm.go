package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"strings"

	"github.com/noble-gase/argon/llmchat"
	"github.com/noble-gase/argon/userlock"
	"github.com/noble-gase/neon/helper"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/card"
	"github.com/redis/go-redis/v9"
)

// ConfirmCard 配置人工确认卡片。决定值会与 params[ParamKey] 和回调的 action id
// 做大小写不敏感匹配。
type ConfirmCard struct {
	// TemplateId 是人工确认卡片使用的模板，必填。它必须有两个按钮，回调时通过
	// params 里的值或 action id 上报用户的决定。正文变量 `content` 必须是普通模板
	// 变量（不是流式变量）：投放时写入 prompt，之后用 UpdateCard 按 key 更新。
	TemplateId string

	// ParamKey 是承载决定值的 params 键名（如 "action"）。留空则跳过按参数解析，
	// 只依据 action id。
	ParamKey string

	// StatusKey 是模板里承载卡片状态的变量名（如 "status"），模板靠它决定按钮是否
	// 显示。只在终态写入对应决定的 Status（成功、过期、失败并开始新对话）。
	// 同步点击和锁忙不改状态，按钮保持可点，避免藏死却恢复失败把用户卡住。
	// 留空则不改模板变量。
	StatusKey string

	// Approve 描述「同意」这个决定。
	Approve ConfirmAction

	// Reject 描述「拒绝」这个决定，参见 Approve。
	Reject ConfirmAction
}

// ConfirmAction 描述一个确认决定（同意或拒绝）。
type ConfirmAction struct {
	// Value 是选中这个决定所用的 params[ParamKey] 值或 action id
	// （大小写不敏感匹配）。
	Value string

	// Status 是该决定落到终态时写入 ConfirmCard.StatusKey 的值（如 "approve"），
	// 用来隐藏按钮。留空则这个决定不改卡片状态。
	Status string
}

// requestConfirmation 投放带同意/拒绝按钮的确认卡片，并把待确认状态持久化，
// 等待卡片按钮回调恢复执行。失败时不写任何卡片文案：调用方要先把 ADK 里的确认
// 撤掉，再连同结果一起收尾。
func (b *Bot) requestConfirmation(ctx context.Context, meta msgMeta, outTrackId, prior, sessionId string, confirm *llmchat.Confirmation) error {
	// 确认卡片内容
	prompt := fmt.Sprintf("即将执行 **%s**", confirm.ToolName)
	if params := formatJSONBlock(confirm.Args); params != "" {
		prompt += "\n\n参数：\n" + params
	}
	if strings.TrimSpace(confirm.Hint) != "" {
		prompt += "\n\n说明：" + confirm.Hint
	}
	if payload := formatJSONBlock(confirm.Payload); payload != "" {
		prompt += "\n\n附加信息：\n" + payload
	}
	prompt += "\n\n请点击下方按钮 **同意** 或 **拒绝**。"

	// 先落库再投卡：反过来的话，投卡成功而落库失败会留下一张点了没反应的卡片。
	confirmOutTrackId := b.card.NewOutTrackId()
	if err := b.card.savePending(ctx, confirmOutTrackId, &pendingConfirm{
		CallId:      confirm.CallId,
		UserId:      meta.userId,
		ConvType:    meta.convType,
		GroupConvId: meta.groupConvId,
		SessionId:   sessionId,
	}); err != nil {
		slog.ErrorContext(ctx, "[dingtalk confirm] card pending save failed", slog.Any("error", err), slog.Any("meta", meta), slog.Any("confirm", confirm))
		return err
	}

	if _, err := b.card.DeliverConfirm(ctx, confirmOutTrackId, meta, prompt); err != nil {
		slog.ErrorContext(ctx, "[dingtalk confirm] card deliver failed", slog.Any("error", err), slog.Any("meta", meta), slog.Any("confirm", confirm))
		if derr := b.card.dropPending(ctx, confirmOutTrackId, meta.userId); derr != nil {
			slog.ErrorContext(ctx, "[dingtalk confirm] drop unsent confirmation failed", slog.String("error", derr.Error()), slog.String("outTrackId", confirmOutTrackId))
		}
		return err
	}

	// 确认卡片确实发出去了，才把原卡指过去
	note := prior
	if strings.TrimSpace(note) != "" {
		note += "\n\n"
	}
	note += fmt.Sprintf("> ⏳ 需确认后才能执行工具「%s」，请查看下方确认卡片。", confirm.ToolName)
	b.settle(ctx, outTrackId, note)
	return nil
}

// confirmCardHandler 处理确认卡片的按钮点击。
//
// 这里只做校验和派发，不改动任何状态：真正的认领、恢复、清理全部在
// resumeConfirmed 的用户锁内完成，否则「取消」「重复点击」「恢复」会各自
// 修改状态而互相踩踏。
func (b *Bot) confirmCardHandler(ctx context.Context, req *card.CardRequest) (*card.CardResponse, error) {
	ctx = helper.CtxWithTraceID(ctx)

	slog.InfoContext(ctx, "[dingtalk confirm] card request", slog.Any("req", req))

	// 先认出是不是自己的确认按钮：卡片回调只有一个全局 router，不能把别人的
	// 交互卡片写成「确认已失效」。
	approved, valid := b.decisionApproved(req)
	if !valid {
		slog.WarnContext(ctx, "[dingtalk confirm] confirm rejected: invalid decision", slog.String("outTrackId", req.OutTrackId))
		return &card.CardResponse{}, nil
	}

	pending, err := b.card.loadPending(ctx, req.OutTrackId)
	if err != nil {
		if errors.Is(err, redis.Nil) {
			// 这次确认已经结束，卡片上写的就是它的结果（「✅ 已同意」等）。
			// 迟到的重复回调绝不能再去改它，否则会把成功终态盖成「已失效」。
			slog.InfoContext(ctx, "[dingtalk confirm] click ignored: confirmation already settled", slog.String("outTrackId", req.OutTrackId))
			return &card.CardResponse{}, nil
		}
		slog.ErrorContext(ctx, "[dingtalk confirm] load pending confirmation failed", slog.String("error", err.Error()), slog.String("outTrackId", req.OutTrackId))
		// 返回错误让钉钉重试，避免 Redis 抖动导致用户点击被静默丢弃
		return nil, err
	}

	// 鉴权：只有发起该确认的用户本人可以决定，避免他人替其批准/拒绝
	if req.UserId != pending.UserId {
		slog.WarnContext(ctx, "[dingtalk confirm] confirm rejected: user mismatch", slog.String("actor", req.UserId), slog.String("owner", pending.UserId))
		return &card.CardResponse{}, nil
	}

	meta := msgMeta{
		userId:      pending.UserId,
		convType:    pending.ConvType,
		groupConvId: pending.GroupConvId,
	}

	// 放到受生命周期管理的 goroutine，让回调快速返回；Stop 会取消并等待它，
	// 避免关闭卡片客户端后仍有后台任务写入。
	if !b.launch(ctx, "resumeConfirmed", func(runCtx context.Context) {
		b.resumeConfirmed(runCtx, meta, req.OutTrackId, approved)
	}) {
		return confirmStatusResponse("> ⚠️ 服务正在停止，请稍后重新点击。"), nil
	}

	// 同步不改卡片。钉钉在回调返回之后才应用 CardResponse，若这里写「处理中」，
	// 会盖掉已经跑完的终态（过期、已处理等快速分支），按钮又被藏掉，用户没有出路。
	// 「处理中」改由 resumeConfirmed 抢到锁、确认记录仍在之后写。
	return &card.CardResponse{}, nil
}

// resumeConfirmed 在用户锁内完成一次确认的全部状态变更。
//
// 锁内顺序：重新读取记录（可能已被取消清理或已被上一次点击处理）→ 写「处理中」
// → 投放回答卡片 → 恢复 ADK → 删除记录或整段丢弃会话。因为和聊天消息、取消
// 共用同一把锁，三者不会交叉；记录的存在性本身就是幂等闸门，不需要额外的租约。
func (b *Bot) resumeConfirmed(ctx context.Context, meta msgMeta, confirmTrackId string, approved bool) {
	// 回答卡可能还没建起来，所以收尾几张取决于 panic 时进行到哪一步
	answerTrackId := ""

	err := b.locked(ctx, meta.userId, func(ctx context.Context) {
		// recover 必须在锁内：abortConfirm 要 ResetAutomatic，不能在解锁后跑。
		resumed := false
		defer func() {
			if r := recover(); r != nil {
				slog.ErrorContext(ctx, "[dingtalk confirm] panic", slog.Any("error", r), slog.String("stack", string(debug.Stack())))
				if answerTrackId != "" {
					b.settle(ctx, answerTrackId, "> ⚠️ 执行过程中出现内部错误，请稍后重试")
				}
				if resumed {
					b.dropConfirm(ctx, confirmTrackId, meta.userId)
					b.finishConfirm(ctx, confirmTrackId, "> ⚠️ 执行未能完成，请重新发起对话。", approved)
					return
				}
				b.abortConfirm(ctx, meta.userId, confirmTrackId, approved)
			}
		}()

		// 锁内重新读取：并发点击的第二次、以及会话已被重置的情况都会在这里被挡住
		pending, err := b.card.loadPending(ctx, confirmTrackId)
		if err != nil {
			if errors.Is(err, redis.Nil) {
				// 记录已被移除：要么上一次点击处理完并写好了终态（如「✅ 已同意」），
				// 要么取消流程已把卡片统一置为「已随对话取消」。这里绝不能再写
				// 「已失效」，否则会覆盖刚写上的结果文案。静默返回即可。
				slog.InfoContext(ctx, "[dingtalk confirm] resume skipped: confirmation already settled elsewhere", slog.String("outTrackId", confirmTrackId))
				return
			}
			slog.ErrorContext(ctx, "[dingtalk confirm] reload pending confirmation failed", slog.String("error", err.Error()), slog.String("outTrackId", confirmTrackId))
			b.keepConfirm(ctx, confirmTrackId, confirmRetryText)
			return
		}

		b.updateConfirm(ctx, confirmTrackId, processingText(approved), "")

		outTrackId, err := b.deliverCard(ctx, meta)
		if err != nil {
			// Confirm 还没调用，会话仍停在确认态。发卡失败多是限流/抖动，再点一次
			// 是安全的，不能为此 ResetAutomatic 毁掉整段对话。
			slog.ErrorContext(ctx, "[dingtalk confirm] card create and deliver failed", slog.String("error", err.Error()), slog.Any("meta", meta), slog.String("callId", pending.CallId))
			b.keepConfirm(ctx, confirmTrackId, confirmRetryText)
			return
		}
		answerTrackId = outTrackId

		seq, err := b.chat.Confirm(ctx, meta.userId, pending.SessionId, pending.CallId, approved, nil)
		if errors.Is(err, llmchat.ErrConversationChanged) {
			// 自动会话已跨日轮换，这张卡指向的执行早已被放弃
			slog.InfoContext(ctx, "[dingtalk confirm] confirmation expired", slog.String("outTrackId", confirmTrackId), slog.String("callId", pending.CallId), slog.String("sessionId", pending.SessionId))
			b.dropConfirm(ctx, confirmTrackId, meta.userId)
			b.settle(ctx, outTrackId, expiredText)
			b.finishConfirm(ctx, confirmTrackId, expiredText, approved)
			return
		}
		if errors.Is(err, llmchat.ErrAlreadyConfirmed) {
			// 会话里已经有这次决定了。带副作用的工具不能执行两次，所以以会话为准，
			// 哪怕上一轮的记录清理失败、用户又点了一次。
			slog.InfoContext(ctx, "[dingtalk confirm] confirmation already answered", slog.String("outTrackId", confirmTrackId), slog.String("callId", pending.CallId))
			b.dropConfirm(ctx, confirmTrackId, meta.userId)
			b.settle(ctx, outTrackId, "> ℹ️ 这次确认已经处理过了。")
			b.finishConfirm(ctx, confirmTrackId, "> ℹ️ 这次确认已经处理过了。", approved)
			return
		}
		if errors.Is(err, llmchat.ErrConfirmationNotFound) {
			// 会话匹配但历史里查无此确认，属于防御性兜底（重置和跨日都由
			// ErrConversationChanged 拦截）。把它当成失效入口，而不是邀请用户反复重试。
			slog.InfoContext(ctx, "[dingtalk confirm] confirmation no longer pending", slog.String("outTrackId", confirmTrackId), slog.String("callId", pending.CallId))
			b.dropConfirm(ctx, confirmTrackId, meta.userId)
			b.settle(ctx, outTrackId, staleConfirmText)
			b.finishConfirm(ctx, confirmTrackId, staleConfirmText, approved)
			return
		}
		if err != nil {
			slog.ErrorContext(ctx, "[dingtalk confirm] resume failed", slog.String("error", err.Error()), slog.String("outTrackId", confirmTrackId), slog.String("callId", pending.CallId))
			note := b.abortConfirm(ctx, meta.userId, confirmTrackId, approved)
			b.settle(ctx, outTrackId, "> ⚠️ 出现错误："+err.Error()+"\n\n"+note)
			return
		}

		// 事件流一旦开始消费，ADK 就已经把这次决策写进会话，父 callId 不能再被重放：
		// 后续无论成败，这条确认记录都必须作废，否则重试会重复执行同一个调用。
		resumed = true
		failed := b.handleAnswer(ctx, seq, meta, outTrackId, pending.SessionId)

		// 清理只是为了让卡片不再可点；即便失败也不会重复执行工具，
		// 因为再次点击会被会话历史里的决定挡在 ErrAlreadyConfirmed 上。
		b.dropConfirm(ctx, confirmTrackId, meta.userId)

		if failed {
			slog.ErrorContext(ctx, "[dingtalk confirm] resume completed with failure", slog.String("outTrackId", confirmTrackId), slog.String("callId", pending.CallId))
			b.finishConfirm(ctx, confirmTrackId, "> ⚠️ 执行未能完成，请重新发起对话。", approved)
			return
		}
		b.finishConfirm(ctx, confirmTrackId, decisionText(approved), approved)
	})
	if err != nil {
		// 没拿到锁，什么都没做：不能 ResetAutomatic，也不能覆盖别人已经写上的终态。
		slog.ErrorContext(ctx, "[dingtalk confirm] resume skipped: lock unavailable", slog.String("error", err.Error()), slog.String("outTrackId", confirmTrackId))
		if errors.Is(err, userlock.ErrBusy) {
			b.keepConfirm(ctx, confirmTrackId, "> ⏳ 上一条消息还在处理中，请等它完成后重新点击。")
			return
		}
		b.keepConfirm(ctx, confirmTrackId, confirmRetryText)
	}
}

// abortConfirm 在 Confirm 已经调用但没能恢复时丢掉整段自动会话。必须在用户锁内调用。
func (b *Bot) abortConfirm(ctx context.Context, userId, confirmTrackId string, approved bool) string {
	note := "> 已开始新的对话，请重新发起。"
	if err := b.chat.ResetAutomatic(ctx, userId); err != nil {
		slog.ErrorContext(ctx, "[dingtalk confirm] abort reset failed", slog.String("error", err.Error()), slog.String("userId", userId), slog.String("outTrackId", confirmTrackId))
		// 会话仍停在确认态，按钮留着让用户重试
		b.updateConfirm(ctx, confirmTrackId, confirmAbortResetFailedText, "")
		return confirmAbortResetFailedText
	}

	ids, err := b.card.clearConfirms(ctx, userId)
	b.settleCancelledConfirms(ctx, ids, confirmTrackId)
	if err != nil {
		slog.ErrorContext(ctx, "[dingtalk confirm] abort clear confirmations failed", slog.String("error", err.Error()), slog.String("userId", userId))
	}

	text := confirmAbortText + "\n\n" + note
	b.finishConfirm(ctx, confirmTrackId, text, approved)
	return text
}

// keepConfirm 写回“请重新点击”类文案，但不改按钮状态、也不动会话。用于没抢到
// 锁、以及 Confirm 尚未调用（Redis 读失败、发卡失败）的分支。记录若已不在，
// 说明别人已经收尾，绝不能覆盖终态。
func (b *Bot) keepConfirm(ctx context.Context, confirmTrackId, content string) {
	_, err := b.card.loadPending(ctx, confirmTrackId)
	if errors.Is(err, redis.Nil) {
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "[dingtalk confirm] keep pending check failed", slog.String("error", err.Error()), slog.String("outTrackId", confirmTrackId))
	}
	b.updateConfirm(ctx, confirmTrackId, content, "")
}

// finishConfirm 一次写完确认卡的终态文案和状态，不会出现“文案已终态、按钮还在”
// 的中间态。写失败不影响正确性：pending 已清理，再点是空操作。
func (b *Bot) finishConfirm(ctx context.Context, confirmTrackId, content string, approved bool) {
	b.updateConfirm(ctx, confirmTrackId, content, b.decisionStatus(approved))
}

// updateConfirm 更新确认卡。它的 content 是普通模板变量，只能按 key 更新，走不了
// 回答卡那套流式接口。status 为空则不动状态变量，按钮保持原样。
func (b *Bot) updateConfirm(ctx context.Context, confirmTrackId, content, status string) {
	params := map[string]string{"content": content}
	maps.Copy(params, b.statusParams(status))

	// 和 settle 一样用独立 context：调用方的 ctx 可能已因丢锁或超时而取消
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cardSettleTimeout)
	defer cancel()

	if err := b.card.UpdateParams(ctx, confirmTrackId, params); err != nil {
		slog.ErrorContext(ctx, "[dingtalk confirm] update confirm card failed", slog.String("error", err.Error()), slog.String("outTrackId", confirmTrackId))
	}
}

// statusParams 把一个状态值包成卡片模板变量；StatusKey 或状态为空时不改模板变量。
// 只写这次决定自己的 Status：回退到另一个决定会把「已拒绝」写成「已同意」的样式。
func (b *Bot) statusParams(status string) map[string]string {
	if b.confirm == nil || b.confirm.StatusKey == "" || status == "" {
		return nil
	}
	return map[string]string{b.confirm.StatusKey: status}
}

const confirmAbortText = "> ⚠️ 这次确认未能生效。"

const confirmAbortResetFailedText = "> ⚠️ 这次确认未能生效，且无法开始新对话，请回复「取消」。"

// confirmRetryText 只用于没抢到锁、会话仍停在确认态的提示。锁内失败会直接开始新对话。
const confirmRetryText = "> ⚠️ 上一次确认未能生效，请重新点击。"

const staleConfirmText = "> ℹ️ 这次确认已失效或已经处理，请重新发起。"

const cancelledConfirmText = "> ℹ️ 这次确认已随对话取消。"

// settleCancelledConfirms 把连带清掉的确认卡写成取消文案。skip 是这次要点名
// 另写终态的卡片（abort 自己的那张），空串表示全部都写取消。
func (b *Bot) settleCancelledConfirms(ctx context.Context, trackIds []string, skip string) {
	for _, id := range trackIds {
		if id == skip {
			continue
		}
		// 取消不是拒绝，不能借用 Reject.Status；pending 已清，再点是空操作。
		b.updateConfirm(ctx, id, cancelledConfirmText, "")
	}
}

// expiredText 用于自动会话已轮换、这次确认不再可恢复的情况。和 confirmRetryText
// 相反，这里不能邀请用户重试——再点多少次都不会生效。
const expiredText = "> ⚠️ 这次确认已过期（对话已开始新的一天），请重新发起。"

// dropConfirm 清理确认记录。失败只影响 UX（卡片仍可点），不影响正确性。
func (b *Bot) dropConfirm(ctx context.Context, confirmTrackId, userId string) {
	if err := b.card.dropPending(ctx, confirmTrackId, userId); err != nil {
		slog.ErrorContext(ctx, "[dingtalk confirm] drop confirmation failed", slog.String("error", err.Error()), slog.String("outTrackId", confirmTrackId))
	}
}

// confirmStatusResponse 构造按钮点击后的同步卡片更新，只改文案、不藏按钮。
func confirmStatusResponse(content string) *card.CardResponse {
	return &card.CardResponse{
		CardUpdateOptions: &card.CardUpdateOptions{UpdateCardDataByKey: true},
		CardData: &card.CardDataDto{CardParamMap: map[string]string{
			"content": content,
		}},
	}
}

// decisionStatus 返回做出该决定后卡片要切到的状态。
func (b *Bot) decisionStatus(approved bool) string {
	if b.confirm == nil {
		return ""
	}
	if approved {
		return b.confirm.Approve.Status
	}
	return b.confirm.Reject.Status
}

func processingText(approved bool) string {
	if approved {
		return "> ⏳ 已同意，正在执行..."
	}
	return "> ⏳ 已拒绝，正在处理..."
}

// decisionApproved 从卡片回调中解析用户是否同意。先看配置的 params 键值，
// 再回退看 actionId 列表；二者都用配置里的 Approve.Value/Reject.Value
// 做大小写不敏感匹配。返回 (approved, valid)，valid=false 表示无法识别。
func (b *Bot) decisionApproved(req *card.CardRequest) (approved bool, valid bool) {
	if b.confirm == nil {
		return false, false
	}

	// 1) params[key] 的值
	if b.confirm.ParamKey != "" {
		val := strings.TrimSpace(actionParamString(req, b.confirm.ParamKey))
		if len(val) != 0 {
			if eqFold(val, b.confirm.Approve.Value) {
				return true, true
			}
			if eqFold(val, b.confirm.Reject.Value) {
				return false, true
			}
		}
	}

	// 2) 回退：actionId 列表
	for _, id := range req.CardActionData.CardPrivateData.ActionIdList {
		id = strings.TrimSpace(id)
		if eqFold(id, b.confirm.Approve.Value) {
			return true, true
		}
		if eqFold(id, b.confirm.Reject.Value) {
			return false, true
		}
	}
	return false, false
}

// formatJSONBlock 把任意值格式化为 JSON 代码块，供确认卡片展示。空值（nil、
// 空 map/slice）返回空串。json.MarshalIndent 会按 key 字母序输出，结果稳定。
func formatJSONBlock(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case map[string]any:
		if len(val) == 0 {
			return ""
		}
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return "```json\n" + string(b) + "\n```"
}

// actionParamString 读取卡片回调 params[key]，兼容字符串以外的类型
// （如布尔/数字），统一转成字符串。
func actionParamString(req *card.CardRequest, key string) string {
	v, ok := req.CardActionData.CardPrivateData.Params[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// eqFold 大小写不敏感比较；want 为空时永不命中。
func eqFold(got, want string) bool {
	return want != "" && strings.EqualFold(got, want)
}

func decisionText(approved bool) string {
	if approved {
		return "> ✅ 已同意"
	}
	return "> ❌ 已拒绝"
}
