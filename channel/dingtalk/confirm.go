package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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

	// Approve describes the "approve" decision: the callback value that selects
	// it and the card params written back after it.
	Approve ConfirmAction

	// Reject describes the "reject" decision. See Approve.
	Reject ConfirmAction
}

// ConfirmAction describes one confirmation decision (approve or reject): the
// callback value that selects it, and the card params written back to the
// confirmation card after it.
type ConfirmAction struct {
	// Value is the params[ParamKey] value or action id that selects this
	// decision (matched case-insensitively).
	Value string

	// Params are extra card params merged back into the confirmation card
	// (update-by-key) after this decision, typically to hide the action buttons
	// or switch the card to a "done" state. Keys/values are specific to your
	// card template (e.g. {"status": "approve"}). Nil leaves the card unchanged.
	Params map[string]string
}

// requestConfirmation 收尾当前卡片，并投放带同意/拒绝按钮的确认卡片，
// 同时把待确认状态持久化，等待卡片按钮回调恢复执行。
func (b *Bot) requestConfirmation(ctx context.Context, meta msgMeta, outTrackId, prior string, confirm *llmchat.Confirmation) {
	// 原卡片收尾提示
	note := prior
	if strings.TrimSpace(note) != "" {
		note += "\n\n"
	}
	note += fmt.Sprintf("> ⏳ 需确认后才能执行工具「%s」，请查看下方确认卡片。", confirm.ToolName)
	b.card.StreamingUpdate(ctx, outTrackId, note, true)

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

	confirmOutTrackId, err := b.card.DeliverConfirm(ctx, meta, prompt)
	if err != nil {
		slog.ErrorContext(ctx, "[dingtalk confirm] card deliver failed", slog.Any("error", err), slog.Any("meta", meta), slog.Any("confirm", confirm))
		return
	}

	if err := b.card.savePending(ctx, confirmOutTrackId, &pendingConfirm{
		CallId:      confirm.CallId,
		UserId:      meta.userId,
		ConvType:    meta.convType,
		GroupConvId: meta.groupConvId,
	}); err != nil {
		slog.ErrorContext(ctx, "[dingtalk confirm] card pending save failed", slog.Any("error", err), slog.Any("meta", meta), slog.Any("confirm", confirm))
	}
}

// cardCallback 处理确认卡片的按钮点击，恢复被暂停的工具执行。
func (b *Bot) confirmCardHandler(ctx context.Context, req *card.CardRequest) (*card.CardResponse, error) {
	ctx = helper.CtxWithTraceId(ctx)

	slog.InfoContext(ctx, "[dingtalk confirm] card request", slog.Any("req", req))

	pending, err := b.card.loadPending(ctx, req.OutTrackId)
	if err != nil {
		if errors.Is(err, redis.Nil) {
			// 非确认卡片或已处理，忽略
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

	pending, err = b.card.consumePending(ctx, req.OutTrackId)
	if err != nil {
		if errors.Is(err, redis.Nil) {
			// 已被并发点击消费，忽略
			return &card.CardResponse{}, nil
		}
		slog.ErrorContext(ctx, "[dingtalk confirm] consume pending confirmation failed", slog.String("error", err.Error()), slog.String("outTrackId", req.OutTrackId))
		// 返回错误让钉钉重试，避免用户点击后状态被静默吞掉
		return nil, err
	}
	if req.UserId != pending.UserId {
		slog.WarnContext(ctx, "[dingtalk confirm] confirm rejected: consumed user mismatch", slog.String("actor", req.UserId), slog.String("owner", pending.UserId))
		return &card.CardResponse{}, nil
	}

	meta := msgMeta{
		userId:      pending.UserId,
		convType:    pending.ConvType,
		groupConvId: pending.GroupConvId,
	}

	// 后台恢复执行并投放回答卡片。放到 goroutine 让回调尽快返回，避免钉钉因回调
	// 超时而丢弃下面用于更新确认卡片（文案 + 隐藏按钮）的响应。
	go b.resumeConfirmed(context.WithoutCancel(ctx), meta, pending.CallId, approved)

	// 立即返回卡片更新：更新文案并回写模板变量（隐藏同意/拒绝按钮）。
	return b.confirmResponse(approved), nil
}

// resumeConfirmed 在后台恢复被暂停的工具执行，并把回答投放到新卡片。
func (b *Bot) resumeConfirmed(ctx context.Context, meta msgMeta, callId string, approved bool) {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	// 先投放回答卡片，拿到 outTrackId 以便后续出错时也能反馈给用户。
	var (
		outTrackId string
		err        error
	)
	if meta.convType == "2" {
		outTrackId, err = b.card.CreateAndDeliverGroup(ctx, meta.userId, meta.groupConvId)
	} else {
		outTrackId, err = b.card.CreateAndDeliverRobot(ctx, meta.userId)
	}
	if err != nil {
		slog.ErrorContext(ctx, "[dingtalk confirm] card create and deliver failed", slog.String("error", err.Error()), slog.Any("meta", meta), slog.String("callId", callId), slog.Bool("approved", approved))
		return
	}

	defer b.recover(ctx, "resumeConfirmed", outTrackId)

	seq, err := b.chat.Confirm(ctx, meta.userId, callId, approved, nil)
	if err != nil {
		b.card.StreamingUpdate(ctx, outTrackId, "> ⚠️ 出现错误："+err.Error(), true)
		return
	}
	b.handleAnswer(ctx, seq, meta, outTrackId)
}

// confirmResponse 构造按钮点击后的确认卡片更新响应：更新文案（content），
// 并在配置了 Approve/Reject.Params 时把对应模板变量按 key 合并回卡片，用于隐藏
// 按钮、切换状态等。
func (b *Bot) confirmResponse(approved bool) *card.CardResponse {
	params := map[string]string{"content": decisionText(approved)}
	if b.confirm != nil {
		src := b.confirm.Reject.Params
		if approved {
			src = b.confirm.Approve.Params
		}
		maps.Copy(params, src)
	}
	return &card.CardResponse{
		CardUpdateOptions: &card.CardUpdateOptions{UpdateCardDataByKey: true},
		CardData:          &card.CardDataDto{CardParamMap: params},
	}
}

// decisionApproved 从卡片回调中解析用户是否同意。先看配置的 params 键值，
// 再回退看 actionId 列表；二者都用配置里的 Approve.Value/Reject.Value
// 做大小写不敏感匹配。返回 (approved, valid)，valid=false 表示无法识别。
func (b *Bot) decisionApproved(req *card.CardRequest) (approved bool, valid bool) {
	if b.confirm == nil {
		return false, false
	}

	// 1) params[key] 的值
	if len(b.confirm.ParamKey) != 0 {
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
		return "> ✅ 已同意，正在执行..."
	}
	return "> ❌ 已拒绝"
}
