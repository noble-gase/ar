package openai

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestNormalizeToolCallID(t *testing.T) {
	model := &openaiModel{}

	short := "call_123"
	if got := model.normalizeToolCallId(short); got != short {
		t.Fatalf("short ID changed: got %q, want %q", got, short)
	}

	long := strings.Repeat("a", maxToolCallIdLength+1)
	first := model.normalizeToolCallId(long)
	second := model.normalizeToolCallId(long)
	if first != second {
		t.Fatalf("normalization is not deterministic: %q != %q", first, second)
	}
	if len(first) != maxToolCallIdLength {
		t.Fatalf("normalized ID length = %d, want %d", len(first), maxToolCallIdLength)
	}
	if !strings.HasPrefix(first, "tc_") {
		t.Fatalf("normalized ID %q lacks tc_ prefix", first)
	}
}

// reasoning_content 是 OpenAI 兼容服务的非标准字段，只能从原始 JSON 信封读取；
// 字段缺失、为空或 JSON 非法时必须安静地返回空串，不能让一次响应解析失败。
func TestExtractReasoningContent(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want string
	}{
		"present":      {`{"content":"hi","reasoning_content":"let me think"}`, "let me think"},
		"missing":      {`{"content":"hi"}`, ""},
		"empty value":  {`{"reasoning_content":""}`, ""},
		"empty input":  {``, ""},
		"invalid json": {`{oops`, ""},
		"wrong type":   {`{"reasoning_content":42}`, ""},
	} {
		if got := extractReasoningContent(tc.raw); got != tc.want {
			t.Errorf("%s: extractReasoningContent() = %q, want %q", name, got, tc.want)
		}
	}
}

// 推理文本要以 Thought Part 放在最终答案之前，镜像模型产出 token 的时间顺序；
// 推理 token 数要映射到 ThoughtsTokenCount。
func TestConvertResponseKeepsReasoningFirst(t *testing.T) {
	raw := `{
		"id": "chatcmpl-1", "object": "chat.completion",
		"choices": [{
			"index": 0,
			"finish_reason": "tool_calls",
			"message": {
				"role": "assistant",
				"content": "the answer",
				"reasoning_content": "let me think",
				"tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "tool", "arguments": "{\"a\":1}"}}]
			}
		}],
		"usage": {
			"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30,
			"completion_tokens_details": {"reasoning_tokens": 5}
		}
	}`
	var resp openai.ChatCompletion
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	m := &openaiModel{name: "gpt-test"}
	got, err := m.convertResponse(&resp)
	if err != nil {
		t.Fatalf("convertResponse() error = %v", err)
	}

	parts := got.Content.Parts
	if len(parts) != 3 {
		t.Fatalf("convertResponse() returned %d parts, want 3", len(parts))
	}
	if !parts[0].Thought || parts[0].Text != "let me think" {
		t.Errorf("parts[0] = %+v, want the reasoning as a thought part first", parts[0])
	}
	if parts[1].Thought || parts[1].Text != "the answer" {
		t.Errorf("parts[1] = %+v, want the plain answer text", parts[1])
	}
	fc := parts[2].FunctionCall
	if fc == nil || fc.ID != "call_1" || fc.Name != "tool" || fc.Args["a"] != float64(1) {
		t.Errorf("parts[2] = %+v, want the tool call with parsed args", parts[2])
	}

	if got.UsageMetadata == nil || got.UsageMetadata.ThoughtsTokenCount != 5 || got.UsageMetadata.TotalTokenCount != 30 {
		t.Errorf("UsageMetadata = %+v, want reasoning tokens mapped to ThoughtsTokenCount", got.UsageMetadata)
	}
	if got.FinishReason != genai.FinishReasonStop {
		t.Errorf("FinishReason = %v, want %v for tool_calls", got.FinishReason, genai.FinishReasonStop)
	}
}

func TestConvertResponseRejectsEmptyChoices(t *testing.T) {
	m := &openaiModel{name: "gpt-test"}
	if _, err := m.convertResponse(&openai.ChatCompletion{}); !errors.Is(err, ErrNoChoicesInResponse) {
		t.Fatalf("convertResponse() error = %v, want ErrNoChoicesInResponse", err)
	}
}

// 历史里的 FunctionCall 与 FunctionResponse 必须归一成同一个 ID：超长 ID 各自
// 哈希后若对不上，OpenAI 会因 tool 消息找不到对应的 tool_call 而拒绝请求。
func TestConvertContentNormalizesToolCallIdsConsistently(t *testing.T) {
	m := &openaiModel{name: "gpt-test"}
	longId := strings.Repeat("x", maxToolCallIdLength+10)

	call := &genai.Content{
		Role: "model",
		Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{ID: longId, Name: "tool", Args: map[string]any{"a": 1}},
		}},
	}
	response := &genai.Content{
		Role: "user",
		Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{ID: longId, Name: "tool", Response: map[string]any{"ok": true}},
		}},
	}

	callMsgs, err := m.convertContentToMessages(call)
	if err != nil {
		t.Fatalf("convertContentToMessages(call) error = %v", err)
	}
	respMsgs, err := m.convertContentToMessages(response)
	if err != nil {
		t.Fatalf("convertContentToMessages(response) error = %v", err)
	}

	if len(callMsgs) != 1 || callMsgs[0].OfAssistant == nil || len(callMsgs[0].OfAssistant.ToolCalls) != 1 {
		t.Fatalf("call messages = %+v, want one assistant message with one tool call", callMsgs)
	}
	if len(respMsgs) != 1 || respMsgs[0].OfTool == nil {
		t.Fatalf("response messages = %+v, want one tool message", respMsgs)
	}

	callId := callMsgs[0].OfAssistant.ToolCalls[0].OfFunction.ID
	respId := respMsgs[0].OfTool.ToolCallID
	if callId != respId {
		t.Errorf("tool call ID %q != tool response ID %q, want them normalized identically", callId, respId)
	}
	if len(callId) > maxToolCallIdLength {
		t.Errorf("normalized ID length = %d, want <= %d", len(callId), maxToolCallIdLength)
	}
}

// genai 的 FunctionCallingConfig.Mode 到 OpenAI tool_choice 的映射。
func TestBuildParamsMapsToolChoice(t *testing.T) {
	m := &openaiModel{name: "gpt-test"}

	build := func(t *testing.T, mode genai.FunctionCallingConfigMode, allowed []string) openai.ChatCompletionNewParams {
		t.Helper()
		params, err := m.buildChatCompletionParams(&model.LLMRequest{
			Config: &genai.GenerateContentConfig{
				ToolConfig: &genai.ToolConfig{
					FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: mode, AllowedFunctionNames: allowed},
				},
			},
		}, false)
		if err != nil {
			t.Fatalf("buildChatCompletionParams() error = %v", err)
		}
		return params
	}

	if got := build(t, genai.FunctionCallingConfigModeAuto, nil); got.ToolChoice.OfAuto.Value != "auto" {
		t.Errorf("ModeAuto tool_choice = %+v, want auto", got.ToolChoice)
	}
	if got := build(t, genai.FunctionCallingConfigModeNone, nil); got.ToolChoice.OfAuto.Value != "none" {
		t.Errorf("ModeNone tool_choice = %+v, want none", got.ToolChoice)
	}
	if got := build(t, genai.FunctionCallingConfigModeAny, []string{"a", "b"}); got.ToolChoice.OfAuto.Value != "required" {
		t.Errorf("ModeAny with several names tool_choice = %+v, want required", got.ToolChoice)
	}

	got := build(t, genai.FunctionCallingConfigModeAny, []string{"only"})
	if got.ToolChoice.OfFunctionToolChoice == nil || got.ToolChoice.OfFunctionToolChoice.Function.Name != "only" {
		t.Errorf("ModeAny with one name tool_choice = %+v, want the named function", got.ToolChoice)
	}
}

func TestBuildParamsIncludesSystemInstruction(t *testing.T) {
	m := &openaiModel{name: "gpt-test"}
	params, err := m.buildChatCompletionParams(&model.LLMRequest{
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText("be helpful", genai.RoleUser),
		},
		Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)},
	}, false)
	if err != nil {
		t.Fatalf("buildChatCompletionParams() error = %v", err)
	}

	if len(params.Messages) != 2 || params.Messages[0].OfSystem == nil {
		t.Fatalf("messages = %+v, want the system message first", params.Messages)
	}
	if params.Messages[1].OfUser == nil {
		t.Errorf("messages[1] = %+v, want the user message", params.Messages[1])
	}
}

// 两个兼容性修正：type 统一小写（genai.Schema 会给出大写枚举），object 必须带
// properties（哪怕是空的），否则严格的 OpenAI 兼容解析器会拒绝工具定义。
func TestConvertToFunctionParamsNormalizesSchema(t *testing.T) {
	params := convertToFunctionParams(map[string]any{
		"type": "OBJECT",
		"properties": map[string]any{
			"filter": map[string]any{"type": "OBJECT"},
			"tags":   map[string]any{"type": "ARRAY", "items": map[string]any{"type": "OBJECT"}},
		},
	})

	if params["type"] != "object" {
		t.Errorf("type = %v, want lowercased", params["type"])
	}
	filter := params["properties"].(map[string]any)["filter"].(map[string]any)
	if filter["type"] != "object" {
		t.Errorf("nested type = %v, want lowercased", filter["type"])
	}
	if _, ok := filter["properties"]; !ok {
		t.Error("nested object lacks the required empty properties")
	}
	items := params["properties"].(map[string]any)["tags"].(map[string]any)["items"].(map[string]any)
	if _, ok := items["properties"]; !ok {
		t.Error("array item object lacks the required empty properties")
	}

	// 非 map 的 schema（如 *jsonschema.Schema）走 JSON 往返归一
	converted := convertToFunctionParams(struct {
		Type string `json:"type"`
	}{Type: "OBJECT"})
	if converted["type"] != "object" {
		t.Errorf("struct schema type = %v, want lowercased via JSON round trip", converted["type"])
	}
	if _, ok := converted["properties"]; !ok {
		t.Error("struct object schema lacks the required empty properties")
	}
}

// 不支持的 MIME 类型必须让请求失败，而不是悄悄丢掉内容。
func TestConvertInlineDataToPart(t *testing.T) {
	image, err := convertInlineDataToPart(&genai.Blob{MIMEType: "image/png", Data: []byte{1, 2}})
	if err != nil {
		t.Fatalf("convertInlineDataToPart(image) error = %v", err)
	}
	if image.OfImageURL == nil || !strings.HasPrefix(image.OfImageURL.ImageURL.URL, "data:image/png;base64,") {
		t.Errorf("image part = %+v, want a data URI", image)
	}

	audio, err := convertInlineDataToPart(&genai.Blob{MIMEType: "audio/mpeg", Data: []byte{1, 2}})
	if err != nil {
		t.Fatalf("convertInlineDataToPart(audio) error = %v", err)
	}
	if audio.OfInputAudio == nil || audio.OfInputAudio.InputAudio.Format != "mp3" {
		t.Errorf("audio part = %+v, want mp3 format", audio)
	}

	if _, err := convertInlineDataToPart(&genai.Blob{MIMEType: "video/mp4", Data: []byte{1}}); err == nil {
		t.Error("convertInlineDataToPart(video) error = nil, want unsupported MIME type rejected")
	}
}

func TestConvertUsageMetadata(t *testing.T) {
	if got := convertUsageMetadata(openai.CompletionUsage{}); got != nil {
		t.Errorf("convertUsageMetadata(zero) = %+v, want nil", got)
	}

	got := convertUsageMetadata(openai.CompletionUsage{
		PromptTokens:     10,
		CompletionTokens: 20,
		TotalTokens:      30,
		CompletionTokensDetails: openai.CompletionUsageCompletionTokensDetails{
			ReasoningTokens: 5,
		},
	})
	if got == nil || got.PromptTokenCount != 10 || got.CandidatesTokenCount != 20 || got.TotalTokenCount != 30 || got.ThoughtsTokenCount != 5 {
		t.Errorf("convertUsageMetadata() = %+v, want all counts mapped", got)
	}
}

// 模型给出的非法参数 JSON 不能让整轮解析失败，容错成空参数。
func TestParseJSONArgs(t *testing.T) {
	if got := parseJSONArgs(`{"a":1}`); got["a"] != float64(1) {
		t.Errorf("parseJSONArgs() = %v, want the parsed args", got)
	}
	if got := parseJSONArgs(""); got == nil || len(got) != 0 {
		t.Errorf("parseJSONArgs(empty) = %v, want an empty map", got)
	}
	if got := parseJSONArgs("{oops"); got == nil || len(got) != 0 {
		t.Errorf("parseJSONArgs(invalid) = %v, want an empty map", got)
	}
}
