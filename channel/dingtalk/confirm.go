package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"

	"github.com/noble-gase/argon/llmchat"
	"github.com/noble-gase/neon/helper"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/card"
	"github.com/redis/go-redis/v9"
)

// ConfirmCard configures the Human-in-the-Loop confirmation card. Decision
// values are matched case-insensitively against both params[ParamKey] and the
// callback action ids.
type ConfirmCard struct {
	// TemplateId is the card template used for Human-in-the-Loop confirmation
	// cards. It must expose two buttons whose callback reports the user's
	// decision either via a params value or an action id. If empty,
	// Config.CardTemplateId is used.
	TemplateId string

	// ParamKey is the params key carrying the decision value (e.g. "action").
	// Leave empty to skip param-based parsing and rely on action ids only.
	ParamKey string

	// Approve describes the "approve" decision.
	Approve ConfirmAction

	// Reject describes the "reject" decision. See Approve.
	Reject ConfirmAction
}

// ConfirmAction describes one confirmation decision (approve or reject).
//
// 卡片按钮不会被隐藏：后台恢复失败时用户要能在原卡上重试，所以这里不提供
// 「回写模板变量」的口子，避免被配置成点一次就永久失效。
type ConfirmAction struct {
	// Value is the params[ParamKey] value or action id that selects this
	// decision (matched case-insensitively).
	Value string
}

// requestConfirmation 收尾当前卡片，并投放带同意/拒绝按钮的确认卡片，
// 同时把待确认状态持久化，等待卡片按钮回调恢复执行。
func (b *Bot) requestConfirmation(ctx context.Context, meta msgMeta, outTrackId, prior, sessionId string, confirm *llmchat.Confirmation) error {
	// 原卡片收尾提示
	note := prior
	if strings.TrimSpace(note) != "" {
		note += "\n\n"
	}
	note += fmt.Sprintf("> ⏳ 需确认后才能执行工具「%s」，请查看下方确认卡片。", confirm.ToolName)
	b.settle(ctx, outTrackId, note)

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
	// 落库成功而投卡失败只会留下一条无人认领的记录，一小时后自行过期。
	confirmOutTrackId := b.card.NewOutTrackId()
	if err := b.card.savePending(ctx, confirmOutTrackId, &pendingConfirm{
		CallId:      confirm.CallId,
		UserId:      meta.userId,
		ConvType:    meta.convType,
		GroupConvId: meta.groupConvId,
		SessionId:   sessionId,
		Prompt:      prompt,
	}); err != nil {
		slog.ErrorContext(ctx, "[dingtalk confirm] card pending save failed", slog.Any("error", err), slog.Any("meta", meta), slog.Any("confirm", confirm))
		b.settle(ctx, outTrackId, prior+"\n\n> ⚠️ 无法发起人工确认，请重新发起。")
		return err
	}

	if _, err := b.card.DeliverConfirm(ctx, confirmOutTrackId, meta, prompt); err != nil {
		slog.ErrorContext(ctx, "[dingtalk confirm] card deliver failed", slog.Any("error", err), slog.Any("meta", meta), slog.Any("confirm", confirm))
		if derr := b.card.dropPending(ctx, confirmOutTrackId, meta.userId); derr != nil {
			slog.ErrorContext(ctx, "[dingtalk confirm] drop unsent confirmation failed", slog.String("error", derr.Error()), slog.String("outTrackId", confirmOutTrackId))
		}
		b.settle(ctx, outTrackId, prior+"\n\n> ⚠️ 无法发起人工确认，请重新发起。")
		return err
	}
	return nil
}

// confirmCardHandler 处理确认卡片的按钮点击。
//
// 这里只做校验和派发，不改动任何状态：真正的认领、恢复、清理全部在
// resumeConfirmed 的用户锁内完成，否则「取消」「重复点击」「恢复」会各自
// 修改状态而互相踩踏。
func (b *Bot) confirmCardHandler(ctx context.Context, req *card.CardRequest) (*card.CardResponse, error) {
	ctx = helper.CtxWithTraceId(ctx)

	slog.InfoContext(ctx, "[dingtalk confirm] card request", slog.Any("req", req))

	pending, err := b.card.loadPending(ctx, req.OutTrackId)
	if err != nil {
		if errors.Is(err, redis.Nil) {
			// 非确认卡片、已处理，或会话已重置，忽略
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

	approved, valid := b.decisionApproved(req)
	if !valid {
		slog.WarnContext(ctx, "[dingtalk confirm] confirm rejected: invalid decision", slog.String("outTrackId", req.OutTrackId))
		return &card.CardResponse{}, nil
	}

	meta := msgMeta{
		userId:      pending.UserId,
		convType:    pending.ConvType,
		groupConvId: pending.GroupConvId,
	}

	// 放到 goroutine 让回调尽快返回，避免钉钉因回调超时而丢弃下面的卡片更新响应。
	go b.resumeConfirmed(context.WithoutCancel(ctx), meta, req.OutTrackId, approved)

	// 立即返回卡片更新：更新文案并回写模板变量（隐藏同意/拒绝按钮）。
	return b.confirmResponse(approved), nil
}

// resumeConfirmed 在用户锁内完成一次确认的全部状态变更。
//
// 锁内顺序：重新读取记录（可能已被取消清理或已被上一次点击处理）→ 投放回答卡片
// → 恢复 ADK → 删除记录或重投确认卡。因为和聊天消息、取消共用同一把锁，
// 三者不会交叉；记录的存在性本身就是幂等闸门，不需要额外的租约。
func (b *Bot) resumeConfirmed(ctx context.Context, meta msgMeta, confirmTrackId string, approved bool) {
	err := b.locked(ctx, meta.userId, func(ctx context.Context) {
		// 锁内重新读取：并发点击的第二次、以及会话已被重置的情况都会在这里被挡住
		pending, err := b.card.loadPending(ctx, confirmTrackId)
		if err != nil {
			if !errors.Is(err, redis.Nil) {
				slog.ErrorContext(ctx, "[dingtalk confirm] reload pending confirmation failed", slog.String("error", err.Error()), slog.String("outTrackId", confirmTrackId))
				b.settle(ctx, confirmTrackId, confirmRetryText)
			}
			return
		}

		outTrackId, err := b.deliverCard(ctx, meta)
		if err != nil {
			// ADK 还没被调用，记录原样保留，用户在原卡上再点一次即可
			slog.ErrorContext(ctx, "[dingtalk confirm] card create and deliver failed", slog.String("error", err.Error()), slog.Any("meta", meta), slog.String("callId", pending.CallId))
			b.settle(ctx, confirmTrackId, confirmRetryText)
			return
		}

		// panic 时两张卡都要收尾：只收回答卡的话，确认卡会永远停在「处理中」
		defer func() {
			if r := recover(); r != nil {
				slog.ErrorContext(ctx, "[dingtalk confirm] panic", slog.Any("error", r), slog.String("stack", string(debug.Stack())))
				b.settle(ctx, outTrackId, "> ⚠️ 执行过程中出现内部错误，请稍后重试")
				b.settle(ctx, confirmTrackId, confirmRetryText)
			}
		}()

		seq, err := b.chat.Confirm(ctx, meta.userId, pending.SessionId, pending.CallId, approved, nil)
		if errors.Is(err, llmchat.ErrConversationChanged) {
			// 自动会话已跨日轮换，这张卡指向的执行早已被放弃
			slog.InfoContext(ctx, "[dingtalk confirm] confirmation expired", slog.String("outTrackId", confirmTrackId), slog.String("callId", pending.CallId), slog.String("sessionId", pending.SessionId))
			b.dropConfirm(ctx, confirmTrackId, meta.userId)
			b.settle(ctx, outTrackId, expiredText)
			b.settle(ctx, confirmTrackId, expiredText)
			return
		}
		if errors.Is(err, llmchat.ErrAlreadyConfirmed) {
			// 会话里已经有这次决定了。带副作用的工具不能执行两次，所以以会话为准，
			// 哪怕上一轮的记录清理失败、用户又点了一次。
			slog.InfoContext(ctx, "[dingtalk confirm] confirmation already answered", slog.String("outTrackId", confirmTrackId), slog.String("callId", pending.CallId))
			b.dropConfirm(ctx, confirmTrackId, meta.userId)
			b.settle(ctx, outTrackId, "> ℹ️ 这次确认已经处理过了。")
			b.settle(ctx, confirmTrackId, "> ℹ️ 这次确认已经处理过了。")
			return
		}
		if err != nil {
			// 同上：ADK 尚未开始恢复，这次确认还可以重来
			slog.ErrorContext(ctx, "[dingtalk confirm] resume failed", slog.String("error", err.Error()), slog.String("outTrackId", confirmTrackId), slog.String("callId", pending.CallId))
			b.settle(ctx, outTrackId, "> ⚠️ 出现错误："+err.Error())
			b.settle(ctx, confirmTrackId, confirmRetryText)
			return
		}

		// 事件流一旦开始消费，ADK 就已经把这次决策写进会话，父 callId 不能再被重放：
		// 后续无论成败，这条确认记录都必须作废，否则重试会重复执行同一个调用。
		got := b.handleAnswer(ctx, seq, meta, outTrackId, pending.SessionId)

		// 清理只是为了让卡片不再可点；即便失败也不会重复执行工具，
		// 因为再次点击会被会话历史里的决定挡在 ErrAlreadyConfirmed 上。
		b.dropConfirm(ctx, confirmTrackId, meta.userId)

		if got == outcomeFailed {
			slog.ErrorContext(ctx, "[dingtalk confirm] resume completed with failure", slog.String("outTrackId", confirmTrackId), slog.String("callId", pending.CallId))
			b.settle(ctx, confirmTrackId, "> ⚠️ 执行未能完成，请重新发起对话。")
			return
		}
		b.settle(ctx, confirmTrackId, decisionText(approved))
	})
	if err != nil {
		// 没拿到锁，什么都没做：记录原样保留，提示用户重新点击
		slog.ErrorContext(ctx, "[dingtalk confirm] resume skipped: lock unavailable", slog.String("error", err.Error()), slog.String("outTrackId", confirmTrackId))
		b.settle(ctx, confirmTrackId, confirmRetryText)
	}
}

// confirmRetryText 是确认未生效时写回原卡片的文案。按钮始终保留，用户直接再点
// 一次即可重试，因此不需要「失败后另投一张确认卡」这条补偿链路。
const confirmRetryText = "> ⚠️ 上一次确认未能生效，请重新点击。"

// expiredText 用于自动会话已轮换、这次确认不再可恢复的情况。和 confirmRetryText
// 相反，这里不能邀请用户重试——再点多少次都不会生效。
const expiredText = "> ⚠️ 这次确认已过期（对话已开始新的一天），请重新发起。"

// dropConfirm 清理确认记录。失败只影响 UX（卡片仍可点），不影响正确性。
func (b *Bot) dropConfirm(ctx context.Context, confirmTrackId, userId string) {
	if err := b.card.dropPending(ctx, confirmTrackId, userId); err != nil {
		slog.ErrorContext(ctx, "[dingtalk confirm] drop confirmation failed", slog.String("error", err.Error()), slog.String("outTrackId", confirmTrackId))
	}
}

// confirmResponse 构造按钮点击后的同步卡片更新：只把文案改成「处理中」。
//
// 刻意不隐藏按钮：后台恢复此刻尚未开始，一旦失败就得让用户能在原卡上重试。
// 最终状态由后台恢复结束后写回。重复点击不会重复执行——锁内的记录存在性检查会挡住。
func (b *Bot) confirmResponse(approved bool) *card.CardResponse {
	return &card.CardResponse{
		CardUpdateOptions: &card.CardUpdateOptions{UpdateCardDataByKey: true},
		CardData: &card.CardDataDto{CardParamMap: map[string]string{
			"content": processingText(approved),
		}},
	}
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
