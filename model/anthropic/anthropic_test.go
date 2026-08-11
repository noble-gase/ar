package anthropic

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// 开启扩展思考后，thinking 块必须连同签名一起随历史原样回传：带 tool_use 的
// assistant 消息若缺失 thinking 块，Anthropic 会直接 400。
func TestConvertContentRoundTripsThinkingBlocks(t *testing.T) {
	m := &anthropicModel{name: "claude-test", maxOutputTokens: 8192, thinkBudgetTokens: 2048}
	content := &genai.Content{
		Role: "model",
		Parts: []*genai.Part{
			{Text: "let me think", Thought: true, ThoughtSignature: []byte("sig-1")},
			{Thought: true, ThoughtSignature: []byte(redactedThinkingMarker + "opaque-data")},
			{Text: "the answer"},
			{FunctionCall: &genai.FunctionCall{ID: "toolu_1", Name: "tool", Args: map[string]any{"a": 1}}},
		},
	}

	msg, err := m.convertContentToMessage(content)
	if err != nil {
		t.Fatalf("convertContentToMessage() error = %v", err)
	}
	if len(msg.Content) != 4 {
		t.Fatalf("convertContentToMessage() returned %d blocks, want 4", len(msg.Content))
	}
	thinking := msg.Content[0].OfThinking
	if thinking == nil || thinking.Signature != "sig-1" || thinking.Thinking != "let me think" {
		t.Errorf("block[0] = %+v, want the thinking block with its signature intact", msg.Content[0])
	}
	redacted := msg.Content[1].OfRedactedThinking
	if redacted == nil || redacted.Data != "opaque-data" {
		t.Errorf("block[1] = %+v, want the redacted thinking data restored", msg.Content[1])
	}
	if msg.Content[2].OfText == nil || msg.Content[2].OfText.Text != "the answer" {
		t.Errorf("block[2] = %+v, want the text block preserved", msg.Content[2])
	}
	if msg.Content[3].OfToolUse == nil {
		t.Errorf("block[3] = %+v, want the tool_use block preserved", msg.Content[3])
	}
}

// 无法通过 API 校验的思考只能丢弃，且绝不能变成可见文本回传。
func TestConvertContentDropsUnsendableThoughts(t *testing.T) {
	signed := &genai.Content{
		Role: "model",
		Parts: []*genai.Part{
			{Text: "secret reasoning", Thought: true, ThoughtSignature: []byte("sig-1")},
			{Text: "visible"},
		},
	}
	unsigned := &genai.Content{
		Role: "model",
		Parts: []*genai.Part{
			{Text: "secret reasoning", Thought: true},
			{Text: "visible"},
		},
	}

	// 关闭扩展思考：API 不接受 thinking 块，带签名的也要丢弃
	disabled := &anthropicModel{name: "claude-test", maxOutputTokens: 8192}
	// 开启扩展思考但没有签名（如其它模型产生的思考）：无法伪造签名
	enabled := &anthropicModel{name: "claude-test", maxOutputTokens: 8192, thinkBudgetTokens: 2048}

	for name, tc := range map[string]struct {
		m       *anthropicModel
		content *genai.Content
	}{
		"thinking disabled": {disabled, signed},
		"missing signature": {enabled, unsigned},
	} {
		msg, err := tc.m.convertContentToMessage(tc.content)
		if err != nil {
			t.Fatalf("%s: convertContentToMessage() error = %v", name, err)
		}
		if len(msg.Content) != 1 || msg.Content[0].OfText == nil || msg.Content[0].OfText.Text != "visible" {
			t.Errorf("%s: blocks = %+v, want only the visible text block", name, msg.Content)
		}
	}
}

// 响应里的 thinking / redacted_thinking 块要转成带签名的 Thought Part，
// 否则回传历史时无从还原。
func TestConvertResponseKeepsThinkingBlocks(t *testing.T) {
	raw := `{
		"id": "msg_1", "type": "message", "role": "assistant",
		"content": [
			{"type": "thinking", "thinking": "let me think", "signature": "sig-abc"},
			{"type": "redacted_thinking", "data": "opaque-data"},
			{"type": "text", "text": "the answer"},
			{"type": "tool_use", "id": "toolu_1", "name": "tool", "input": {"a": 1}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 10, "output_tokens": 20}
	}`
	var resp sdk.Message
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	m := &anthropicModel{name: "claude-test", thinkBudgetTokens: 2048}
	got, err := m.convertResponse(&resp)
	if err != nil {
		t.Fatalf("convertResponse() error = %v", err)
	}
	parts := got.Content.Parts
	if len(parts) != 4 {
		t.Fatalf("convertResponse() returned %d parts, want 4", len(parts))
	}
	if !parts[0].Thought || parts[0].Text != "let me think" || string(parts[0].ThoughtSignature) != "sig-abc" {
		t.Errorf("parts[0] = %+v, want the thinking part with its signature", parts[0])
	}
	if !parts[1].Thought || string(parts[1].ThoughtSignature) != redactedThinkingMarker+"opaque-data" {
		t.Errorf("parts[1] = %+v, want the redacted thinking data kept", parts[1])
	}
	if parts[2].Text != "the answer" || parts[2].Thought {
		t.Errorf("parts[2] = %+v, want the plain text part", parts[2])
	}
	if parts[3].FunctionCall == nil || parts[3].FunctionCall.ID != "toolu_1" {
		t.Errorf("parts[3] = %+v, want the tool call part", parts[3])
	}
}

// 思考预算的约束（>= 1024 且严格小于 max_tokens）要在发请求前报清晰的错误，
// 而不是让 API 返回一个含糊的 400。
func TestBuildMessageParamsValidatesThinkingBudget(t *testing.T) {
	for name, m := range map[string]*anthropicModel{
		"budget below minimum":   {name: "claude-test", maxOutputTokens: 8192, thinkBudgetTokens: 512},
		"budget over max tokens": {name: "claude-test", maxOutputTokens: 2048, thinkBudgetTokens: 4096},
	} {
		if _, err := m.buildMessageParams(&model.LLMRequest{}); err == nil || !strings.Contains(err.Error(), "ThinkBudgetTokens") {
			t.Errorf("%s: buildMessageParams() error = %v, want a ThinkBudgetTokens error", name, err)
		}
	}
}

// 扩展思考与强制工具调用互斥，要在发请求前报清晰的错误；静默降级成 auto 会让
// 「必须调工具」的语义失效而调用方毫不知情。
func TestThinkingRejectsForcedToolUse(t *testing.T) {
	m := &anthropicModel{name: "claude-test", maxOutputTokens: 8192, thinkBudgetTokens: 2048}
	req := &model.LLMRequest{Config: &genai.GenerateContentConfig{
		ToolConfig: &genai.ToolConfig{
			FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny},
		},
	}}

	if _, err := m.buildMessageParams(req); err == nil || !strings.Contains(err.Error(), "forced tool use") {
		t.Errorf("buildMessageParams(ModeAny) error = %v, want the conflict rejected upfront", err)
	}

	// ModeAuto 与思考兼容，不受影响
	req.Config.ToolConfig.FunctionCallingConfig.Mode = genai.FunctionCallingConfigModeAuto
	if _, err := m.buildMessageParams(req); err != nil {
		t.Errorf("buildMessageParams(ModeAuto) error = %v, want nil", err)
	}
}

// 扩展思考开启时 Anthropic 强制 temperature 为 1，采样参数必须被忽略，
// 否则请求直接 400。
func TestThinkingIgnoresSamplingParams(t *testing.T) {
	m := &anthropicModel{name: "claude-test", maxOutputTokens: 8192, thinkBudgetTokens: 2048}
	temperature, topP := float32(0.2), float32(0.9)
	req := &model.LLMRequest{Config: &genai.GenerateContentConfig{Temperature: &temperature, TopP: &topP}}

	params, err := m.buildMessageParams(req)
	if err != nil {
		t.Fatalf("buildMessageParams() error = %v", err)
	}
	if params.Temperature.Valid() {
		t.Errorf("Temperature = %v, want unset while extended thinking is enabled", params.Temperature)
	}
	if params.TopP.Valid() {
		t.Errorf("TopP = %v, want unset while extended thinking is enabled", params.TopP)
	}
	if params.Thinking.OfEnabled == nil || params.Thinking.OfEnabled.BudgetTokens != 2048 {
		t.Errorf("Thinking = %+v, want the budget applied", params.Thinking)
	}
}

// 孤立 tool_use 被剔除后，只剩 thinking 的 assistant 消息必须整条丢弃：那段
// 思考描述的是已被删掉的工具调用，单独回传轻则污染上下文，重则被 API 拒绝
// （thinking-only 的消息落在末尾还会撞上「扩展思考不允许 prefill」）。
func TestRepairMessageHistoryDropsThinkingOnlyMessage(t *testing.T) {
	messages := []sdk.MessageParam{
		{
			Role: sdk.MessageParamRoleAssistant,
			Content: []sdk.ContentBlockParamUnion{
				sdk.NewThinkingBlock("sig-1", "about to call the tool"),
				sdk.NewToolUseBlock("orphan", map[string]any{}, "tool"),
			},
		},
		{
			Role:    sdk.MessageParamRoleUser,
			Content: []sdk.ContentBlockParamUnion{sdk.NewTextBlock("next question")},
		},
	}

	got := repairMessageHistory(messages)
	if len(got) != 1 {
		t.Fatalf("repairMessageHistory() returned %d messages, want only the user message", len(got))
	}
	if got[0].Role != sdk.MessageParamRoleUser {
		t.Errorf("remaining message role = %v, want the user message", got[0].Role)
	}
}

// thinking 之外还有实质内容（文本）时，消息保留，thinking 块也一并保留。
func TestRepairMessageHistoryKeepsThinkingWithText(t *testing.T) {
	messages := []sdk.MessageParam{{
		Role: sdk.MessageParamRoleAssistant,
		Content: []sdk.ContentBlockParamUnion{
			sdk.NewThinkingBlock("sig-1", "reasoning"),
			sdk.NewTextBlock("the answer"),
			sdk.NewToolUseBlock("orphan", map[string]any{}, "tool"),
		},
	}}

	got := repairMessageHistory(messages)
	if len(got) != 1 {
		t.Fatalf("repairMessageHistory() returned %d messages, want 1", len(got))
	}
	if len(got[0].Content) != 2 || got[0].Content[0].OfThinking == nil || got[0].Content[1].OfText == nil {
		t.Errorf("blocks = %+v, want thinking and text kept with the orphan tool_use removed", got[0].Content)
	}
}

func TestRepairMessageHistoryFiltersBothSides(t *testing.T) {
	messages := []sdk.MessageParam{
		{
			Role: sdk.MessageParamRoleAssistant,
			Content: []sdk.ContentBlockParamUnion{
				sdk.NewTextBlock("before"),
				sdk.NewToolUseBlock("matched", map[string]any{}, "matched_tool"),
				sdk.NewToolUseBlock("orphan_use", map[string]any{}, "orphan_tool"),
			},
		},
		{
			Role: sdk.MessageParamRoleUser,
			Content: []sdk.ContentBlockParamUnion{
				sdk.NewToolResultBlock("matched", `{"ok":true}`, false),
				sdk.NewToolResultBlock("orphan_result", `{"ok":false}`, false),
				sdk.NewTextBlock("after"),
			},
		},
	}

	got := repairMessageHistory(messages)
	if len(got) != 2 {
		t.Fatalf("repairMessageHistory() returned %d messages, want 2", len(got))
	}
	if ids := extractToolUseIds(got[0]); !reflect.DeepEqual(ids, []string{"matched"}) {
		t.Fatalf("tool_use IDs = %v, want [matched]", ids)
	}
	if ids := extractToolResultIds(got[1]); !reflect.DeepEqual(ids, []string{"matched"}) {
		t.Fatalf("tool_result IDs = %v, want [matched]", ids)
	}
	if got[0].Content[0].OfText == nil || got[0].Content[0].OfText.Text != "before" {
		t.Fatal("assistant text block was not preserved")
	}
	if got[1].Content[len(got[1].Content)-1].OfText == nil || got[1].Content[len(got[1].Content)-1].OfText.Text != "after" {
		t.Fatal("user text block was not preserved")
	}
}

func TestRepairMessageHistoryRemovesStandaloneToolResult(t *testing.T) {
	messages := []sdk.MessageParam{{
		Role: sdk.MessageParamRoleUser,
		Content: []sdk.ContentBlockParamUnion{
			sdk.NewToolResultBlock("orphan", `{"ok":false}`, false),
			sdk.NewTextBlock("keep me"),
		},
	}}

	got := repairMessageHistory(messages)
	if len(got) != 1 {
		t.Fatalf("repairMessageHistory() returned %d messages, want 1", len(got))
	}
	if ids := extractToolResultIds(got[0]); len(ids) != 0 {
		t.Fatalf("tool_result IDs = %v, want none", ids)
	}
	if len(got[0].Content) != 1 || got[0].Content[0].OfText == nil || got[0].Content[0].OfText.Text != "keep me" {
		t.Fatal("non-tool content was not preserved")
	}
}

func TestRepairMessageHistoryKeepsMatchedPair(t *testing.T) {
	messages := []sdk.MessageParam{
		{
			Role:    sdk.MessageParamRoleAssistant,
			Content: []sdk.ContentBlockParamUnion{sdk.NewToolUseBlock("call", map[string]any{"q": "value"}, "tool")},
		},
		{
			Role:    sdk.MessageParamRoleUser,
			Content: []sdk.ContentBlockParamUnion{sdk.NewToolResultBlock("call", `{"ok":true}`, false)},
		},
	}

	got := repairMessageHistory(messages)
	if len(got) != 2 {
		t.Fatalf("repairMessageHistory() returned %d messages, want 2", len(got))
	}
	if ids := extractToolUseIds(got[0]); !reflect.DeepEqual(ids, []string{"call"}) {
		t.Fatalf("tool_use IDs = %v, want [call]", ids)
	}
	if ids := extractToolResultIds(got[1]); !reflect.DeepEqual(ids, []string{"call"}) {
		t.Fatalf("tool_result IDs = %v, want [call]", ids)
	}
}

func TestRepairMessageHistoryOrphanAcrossConsecutiveAssistants(t *testing.T) {
	messages := []sdk.MessageParam{
		{
			Role:    sdk.MessageParamRoleAssistant,
			Content: []sdk.ContentBlockParamUnion{sdk.NewToolUseBlock("A", map[string]any{}, "tool")},
		},
		{
			Role:    sdk.MessageParamRoleAssistant,
			Content: []sdk.ContentBlockParamUnion{sdk.NewToolUseBlock("B", map[string]any{}, "tool")},
		},
		{
			Role:    sdk.MessageParamRoleUser,
			Content: []sdk.ContentBlockParamUnion{sdk.NewToolResultBlock("B", `{"ok":true}`, false)},
		},
	}

	// A 没有配对的 tool_result，它那条（已变空的）assistant 消息会被丢弃；
	// B 与结果配对，因此保留。
	got := repairMessageHistory(messages)
	if len(got) != 2 {
		t.Fatalf("repairMessageHistory() returned %d messages, want 2", len(got))
	}
	if ids := extractToolUseIds(got[0]); !reflect.DeepEqual(ids, []string{"B"}) {
		t.Fatalf("tool_use IDs = %v, want [B]", ids)
	}
	if ids := extractToolResultIds(got[1]); !reflect.DeepEqual(ids, []string{"B"}) {
		t.Fatalf("tool_result IDs = %v, want [B]", ids)
	}
}
