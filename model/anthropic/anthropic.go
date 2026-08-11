package anthropic

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"regexp"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/noble-gase/argon/model/common"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

var ErrNoContentInResponse = errors.New("no content in Anthropic response")

// anthropicToolIdPattern 匹配合法的 Anthropic tool_use ID：^[a-zA-Z0-9_-]+$
var anthropicToolIdPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// redactedThinkingMarker 在 ThoughtSignature 里区分 redacted_thinking 与普通
// thinking。genai.Part 没有承载 redacted 数据的专门字段，只能借 ThoughtSignature
// 存放加密数据，回传历史时按这个前缀还原成 redacted_thinking 块。
const redactedThinkingMarker = "redacted_thinking:"

// anthropicModel 用官方 Anthropic Go SDK 实现 model.LLM。
type anthropicModel struct {
	client            *anthropic.Client
	name              string
	maxOutputTokens   int
	thinkBudgetTokens int
}

// HTTPOptions 是 Anthropic 客户端的可选 HTTP 层配置。
type HTTPOptions struct {
	Client  *http.Client
	Headers http.Header
}

// Config 是创建 Model 所需的配置。
type Config struct {
	// APIKey 是 Anthropic 的 API key。留空则使用环境变量 ANTHROPIC_API_KEY。
	APIKey string
	// BaseURL 是 API 基础地址（可选，用于自定义端点）。
	BaseURL string
	// ModelName 是使用的模型（如 "claude-sonnet-4-5-20250929"）。
	ModelName string
	// MaxOutputTokens 设置 Claude 单次响应能生成的默认 token 上限。
	// 它只限制输出，不影响输入/上下文窗口。
	// 为零时默认 4096。
	MaxOutputTokens int
	// ThinkBudgetTokens 开启扩展思考，并设定 Claude 在给出最终答案前
	// 能花在内部推理上的输出 token 数。
	// 思考 token 属于输出 token——Claude 就是把推理当文本生成出来的，
	// 只是不展示给用户（或放在单独的块里返回）。
	// 必须 >= 1024，且严格小于 MaxOutputTokens（含请求级覆盖），
	// 违反时请求会在发出前报错。
	// 开启后 temperature/top_p 会被忽略（Anthropic 强制 temperature 为 1），
	// 思考内容以 Thought Part 返回，并在回传历史时连同签名原样带回。
	// 为零时关闭扩展思考。
	ThinkBudgetTokens int
	// HTTPOptions 是可选的 HTTP 层覆盖配置（如附加请求头）。
	HTTPOptions HTTPOptions
}

// NewModel 返回基于 Anthropic API 的 [model.LLM]。
func NewModel(cfg Config) model.LLM {
	opts := []option.RequestOption{}

	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTPOptions.Client != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPOptions.Client))
	}
	for k, vals := range cfg.HTTPOptions.Headers {
		for _, v := range vals {
			opts = append(opts, option.WithHeaderAdd(k, v))
		}
	}

	client := anthropic.NewClient(opts...)

	return &anthropicModel{
		client:            &client,
		name:              cfg.ModelName,
		maxOutputTokens:   cfg.MaxOutputTokens,
		thinkBudgetTokens: cfg.ThinkBudgetTokens,
	}
}

// Name 返回模型名称（如 "claude-sonnet-4-5-20250929"）。
func (m *anthropicModel) Name() string {
	return m.name
}

// GenerateContent 向 Anthropic 发起请求并返回响应（流式或单条）。
func (m *anthropicModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	if stream {
		return m.generateStream(ctx, req)
	}
	return m.generate(ctx, req)
}

// generate 发起单次请求，产出一个完整响应。
func (m *anthropicModel) generate(ctx context.Context, req *model.LLMRequest) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		params, err := m.buildMessageParams(req)
		if err != nil {
			yield(nil, err)
			return
		}

		resp, err := m.client.Messages.New(ctx, params)
		if err != nil {
			yield(nil, err)
			return
		}

		llmResp, err := m.convertResponse(resp)
		if err != nil {
			yield(nil, err)
			return
		}

		yield(llmResp, nil)
	}
}

// generateStream 发起请求，边到达边产出增量响应，最后再产出一个完整响应。
func (m *anthropicModel) generateStream(ctx context.Context, req *model.LLMRequest) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		params, err := m.buildMessageParams(req)
		if err != nil {
			yield(nil, err)
			return
		}

		stream := m.client.Messages.NewStreaming(ctx, params)
		defer stream.Close()

		message := anthropic.Message{}

		for stream.Next() {
			event := stream.Current()
			if _err := message.Accumulate(event); _err != nil {
				yield(nil, _err)
				return
			}

			// 产出增量文本，思考增量同样实时产出，由下游按 Thought 过滤或展示
			switch eventVariant := event.AsAny().(type) {
			case anthropic.ContentBlockDeltaEvent:
				var part *genai.Part
				switch deltaVariant := eventVariant.Delta.AsAny().(type) {
				case anthropic.TextDelta:
					if deltaVariant.Text != "" {
						part = &genai.Part{Text: deltaVariant.Text}
					}
				case anthropic.ThinkingDelta:
					if deltaVariant.Thinking != "" {
						part = &genai.Part{Text: deltaVariant.Thinking, Thought: true}
					}
				}
				if part != nil {
					llmResp := &model.LLMResponse{
						Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}},
						Partial:      true,
						TurnComplete: false,
					}
					if !yield(llmResp, nil) {
						return
					}
				}
			}
		}

		if err = stream.Err(); err != nil {
			yield(nil, err)
			return
		}

		// 构造最终的聚合响应
		llmResp, err := m.convertResponse(&message)
		if err != nil {
			yield(nil, err)
			return
		}

		llmResp.Partial = false
		llmResp.TurnComplete = true
		yield(llmResp, nil)
	}
}

// buildMessageParams 把 LLMRequest 转换成 Anthropic 的 API 格式（系统提示、消息、工具、配置）。
func (m *anthropicModel) buildMessageParams(req *model.LLMRequest) (anthropic.MessageNewParams, error) {
	// 默认最大 token 数（Anthropic API 必填）
	maxTokens := int64(4096)
	if m.maxOutputTokens > 0 {
		maxTokens = int64(m.maxOutputTokens)
	}
	if req.Config != nil && req.Config.MaxOutputTokens > 0 {
		maxTokens = int64(req.Config.MaxOutputTokens)
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(m.name),
		MaxTokens: maxTokens,
	}

	if m.thinkBudgetTokens > 0 {
		// Anthropic 要求思考预算 >= 1024 且严格小于 max_tokens。配置错误在这里
		// 尽早报出清晰的错误，而不是让 API 返回一个含糊的 400。
		if m.thinkBudgetTokens < 1024 {
			return anthropic.MessageNewParams{}, fmt.Errorf("anthropic: ThinkBudgetTokens must be >= 1024, got %d", m.thinkBudgetTokens)
		}
		if int64(m.thinkBudgetTokens) >= maxTokens {
			return anthropic.MessageNewParams{}, fmt.Errorf("anthropic: ThinkBudgetTokens (%d) must be less than max output tokens (%d)", m.thinkBudgetTokens, maxTokens)
		}
		params.Thinking = anthropic.ThinkingConfigParamUnion{
			OfEnabled: &anthropic.ThinkingConfigEnabledParam{
				BudgetTokens: int64(m.thinkBudgetTokens),
			},
		}
	}

	// 有系统指令则加上
	if req.Config != nil && req.Config.SystemInstruction != nil {
		systemText := extractTextFromContent(req.Config.SystemInstruction)
		if systemText != "" {
			params.System = []anthropic.TextBlockParam{
				{Text: systemText},
			}
		}
	}

	// 转换内容消息
	messages := []anthropic.MessageParam{}
	for _, content := range req.Contents {
		msg, err := m.convertContentToMessage(content)
		if err != nil {
			return anthropic.MessageNewParams{}, err
		}
		if msg != nil {
			messages = append(messages, *msg)
		}
	}

	// 修整消息历史以满足 Anthropic 的要求
	// （每个 tool_use 后面必须紧跟对应的 tool_result）
	messages = repairMessageHistory(messages)
	messages = trimFinalAssistantWhitespace(messages)

	params.Messages = messages

	// 应用配置项
	if req.Config != nil {
		// 扩展思考开启时 Anthropic 强制 temperature 为 1，且不接受 top_p 调整，
		// 传了会直接 400，所以这里静默忽略采样参数。
		if m.thinkBudgetTokens == 0 {
			if req.Config.Temperature != nil {
				params.Temperature = anthropic.Float(float64(*req.Config.Temperature))
			}
			if req.Config.TopP != nil {
				params.TopP = anthropic.Float(float64(*req.Config.TopP))
			}
		}
		if len(req.Config.StopSequences) > 0 {
			params.StopSequences = req.Config.StopSequences
		}

		// 转换工具
		if len(req.Config.Tools) > 0 {
			tools, err := m.convertTools(req.Config.Tools)
			if err != nil {
				return anthropic.MessageNewParams{}, err
			}
			params.Tools = tools
		}

		// ToolConfig → tool_choice
		//
		// 把 genai.FunctionCallingConfig.Mode 映射到 Anthropic 的 tool_choice：
		//   ModeAuto → {type: "auto"} （默认行为，模型可调可不调工具）
		//   ModeAny  → {type: "any"}  （模型必须调工具，用于无法处理纯文本回复的
		//                              agent 循环）
		//   ModeNone → {type: "none"} （本次调用禁用工具，即使传了工具定义）
		//
		// ModeAny 且 AllowedFunctionNames 恰好只有一个名字时，Anthropic 的对应物是
		// {type: "tool", name: "..."}。有多个名字时退回 {type: "any"}，因为 Anthropic
		// 的 tool 变体只接受单个名字而不是列表——和 OpenAI 适配器一样的取舍。需要
		// 多函数白名单的调用方，应当用 ModeAny 加提示词来约束模型在允许的集合内
		// 选择。
		if req.Config.ToolConfig != nil && req.Config.ToolConfig.FunctionCallingConfig != nil {
			fcc := req.Config.ToolConfig.FunctionCallingConfig
			// Anthropic 规定扩展思考不能与强制工具调用（{type:"any"} 或
			// {type:"tool"}）同用。与预算校验同理，在发请求前报清晰的错误；
			// 静默降级成 auto 会让「必须调工具」的语义失效而调用方毫不知情。
			if m.thinkBudgetTokens > 0 && fcc.Mode == genai.FunctionCallingConfigModeAny {
				return anthropic.MessageNewParams{}, fmt.Errorf("anthropic: extended thinking (ThinkBudgetTokens > 0) is incompatible with forced tool use (FunctionCallingConfigModeAny)")
			}
			switch fcc.Mode {
			case genai.FunctionCallingConfigModeAuto:
				params.ToolChoice = anthropic.ToolChoiceUnionParam{
					OfAuto: &anthropic.ToolChoiceAutoParam{},
				}
			case genai.FunctionCallingConfigModeNone:
				params.ToolChoice = anthropic.ToolChoiceUnionParam{
					OfNone: &anthropic.ToolChoiceNoneParam{},
				}
			case genai.FunctionCallingConfigModeAny:
				if len(fcc.AllowedFunctionNames) == 1 {
					params.ToolChoice = anthropic.ToolChoiceParamOfTool(fcc.AllowedFunctionNames[0])
				} else {
					params.ToolChoice = anthropic.ToolChoiceUnionParam{
						OfAny: &anthropic.ToolChoiceAnyParam{},
					}
				}
			}
		}
	}

	return params, nil
}

// convertContentToMessage 把 genai.Content（文本、图片、工具调用/结果）转换成 Anthropic 消息。
func (m *anthropicModel) convertContentToMessage(content *genai.Content) (*anthropic.MessageParam, error) {
	role := convertRoleToAnthropic(content.Role)

	var blocks []anthropic.ContentBlockParamUnion

	for _, part := range content.Parts {
		// 思考内容绝不能作为可见文本回传。带签名的思考块在开启扩展思考时必须
		// 原样回传（Anthropic 要求带 tool_use 的 assistant 消息保留 thinking 块，
		// 否则 400）；关闭思考时 API 不接受 thinking 块，没有签名的思考（比如
		// 其它模型产生的）无法伪造签名，两种情况都只能丢弃。
		if part.Thought {
			if m.thinkBudgetTokens > 0 {
				if block, ok := thinkingBlockFromPart(part); ok {
					blocks = append(blocks, block)
				}
			}
			continue
		}

		if part.Text != "" {
			blocks = append(blocks, anthropic.NewTextBlock(part.Text))
		}

		if part.InlineData != nil {
			block, err := convertInlineDataToBlock(part.InlineData)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, *block)
		}

		if part.FunctionCall != nil {
			input, err := common.MarshalPayload(part.FunctionCall.Args)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal function call args: %w", err)
			}
			blocks = append(blocks, anthropic.ContentBlockParamUnion{
				OfToolUse: &anthropic.ToolUseBlockParam{
					ID:    sanitizeToolId(part.FunctionCall.ID),
					Name:  part.FunctionCall.Name,
					Input: input,
				},
			})
		}

		if part.FunctionResponse != nil {
			responseJSON, err := common.MarshalPayload(part.FunctionResponse.Response)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal function response: %w", err)
			}
			blocks = append(blocks, anthropic.NewToolResultBlock(sanitizeToolId(part.FunctionResponse.ID), string(responseJSON), false))
		}
	}

	if len(blocks) == 0 {
		return nil, nil
	}

	return &anthropic.MessageParam{Role: role, Content: blocks}, nil
}

// convertResponse 把 Anthropic 的响应（文本、tool_use 块、用量）转换成通用的 LLMResponse。
func (m *anthropicModel) convertResponse(resp *anthropic.Message) (*model.LLMResponse, error) {
	content := &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{},
	}

	// 转换内容块。thinking 块必须连同签名一起保留：开启扩展思考后，带 tool_use
	// 的 assistant 消息在回传历史时必须原样带回 thinking 块，否则 API 直接 400。
	for _, block := range resp.Content {
		switch variant := block.AsAny().(type) {
		case anthropic.TextBlock:
			content.Parts = append(content.Parts, &genai.Part{Text: variant.Text})
		case anthropic.ThinkingBlock:
			content.Parts = append(content.Parts, &genai.Part{
				Text:             variant.Thinking,
				Thought:          true,
				ThoughtSignature: []byte(variant.Signature),
			})
		case anthropic.RedactedThinkingBlock:
			content.Parts = append(content.Parts, &genai.Part{
				Thought:          true,
				ThoughtSignature: []byte(redactedThinkingMarker + variant.Data),
			})
		case anthropic.ToolUseBlock:
			content.Parts = append(content.Parts, &genai.Part{
				FunctionCall: &genai.FunctionCall{
					ID:   variant.ID,
					Name: variant.Name,
					Args: convertToolInput(variant.Input),
				},
			})
		}
	}

	// 转换用量元数据
	var usageMetadata *genai.GenerateContentResponseUsageMetadata
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		usageMetadata = &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     int32(resp.Usage.InputTokens),
			CandidatesTokenCount: int32(resp.Usage.OutputTokens),
			TotalTokenCount:      int32(resp.Usage.InputTokens + resp.Usage.OutputTokens),
		}
	}

	return &model.LLMResponse{
		Content:       content,
		UsageMetadata: usageMetadata,
		FinishReason:  convertStopReason(resp.StopReason),
		TurnComplete:  true,
	}, nil
}

// convertTools 把 genai 的工具定义转换成 Anthropic 的工具格式（名称、描述、JSON schema）。
func (m *anthropicModel) convertTools(genaiTools []*genai.Tool) ([]anthropic.ToolUnionParam, error) {
	var tools []anthropic.ToolUnionParam

	for _, genaiTool := range genaiTools {
		if genaiTool == nil {
			continue
		}

		for _, funcDecl := range genaiTool.FunctionDeclarations {
			params := funcDecl.ParametersJsonSchema
			if params == nil {
				params = funcDecl.Parameters
			}

			var inputSchema anthropic.ToolInputSchemaParam
			// Anthropic API 要求 Type 必填，且必须是 "object"
			inputSchema.Type = "object"
			if params != nil {
				// ParametersJsonSchema 通常是 *jsonschema.Schema，而不是 map[string]any。
				// 走一遍 Marshal/Unmarshal 可以把任意具体类型归一成普通 map，便于统一
				// 取字段。如果本来就是 map（比如在 Go 里手写的），直接用，省掉这趟往返。
				var m map[string]any
				if dm, ok := params.(map[string]any); ok {
					m = dm
				} else {
					jsonBytes, err := json.Marshal(params)
					if err == nil {
						_ = json.Unmarshal(jsonBytes, &m) //nolint:errcheck
					}
				}
				if m != nil {
					lowercaseTypes(m)
					if props, ok := m["properties"]; ok {
						inputSchema.Properties = props
					}
					// json.Unmarshal 之后，字符串数组一律是 []any 而不是 []string，与源类型
					// 无关。两种都处理是为了稳妥：
					// []string 覆盖在 Go 里直接构造、没走 JSON 往返的 map；
					// []any 覆盖正常的反序列化路径。
					switch req := m["required"].(type) {
					case []string:
						inputSchema.Required = req
					case []any:
						strs := make([]string, len(req))
						for i, v := range req {
							strs[i] = fmt.Sprint(v)
						}
						inputSchema.Required = strs
					}
				}
			}

			tools = append(tools, anthropic.ToolUnionParam{
				OfTool: &anthropic.ToolParam{
					Name:        funcDecl.Name,
					Description: anthropic.String(funcDecl.Description),
					InputSchema: inputSchema,
				},
			})
		}
	}

	return tools, nil
}

// convertRoleToAnthropic 把 "user"/"model" 映射到 Anthropic 的 role 枚举（user/assistant）。
func convertRoleToAnthropic(role string) anthropic.MessageParamRole {
	switch role {
	case "user":
		return anthropic.MessageParamRoleUser
	case "model":
		return anthropic.MessageParamRoleAssistant
	default:
		return anthropic.MessageParamRoleUser
	}
}

// convertStopReason 把 Anthropic 的停止原因（end_turn、max_tokens、tool_use）映射到 genai.FinishReason。
func convertStopReason(reason anthropic.StopReason) genai.FinishReason {
	switch reason {
	case anthropic.StopReasonEndTurn:
		return genai.FinishReasonStop
	case anthropic.StopReasonMaxTokens:
		return genai.FinishReasonMaxTokens
	case anthropic.StopReasonStopSequence:
		return genai.FinishReasonStop
	case anthropic.StopReasonToolUse:
		return genai.FinishReasonStop
	default:
		return genai.FinishReasonUnspecified
	}
}

// convertToolInput 把工具入参转换成 map[string]any，用于存进 genai.FunctionCall.Args。
// 在接收 Anthropic 响应里的 tool_use 块时使用。
func convertToolInput(input any) map[string]any {
	if input == nil {
		return map[string]any{}
	}
	if m, ok := input.(map[string]any); ok {
		return m
	}

	// 取 JSON 字节：是 json.RawMessage 就直接用，否则做一次序列化
	var data []byte
	if raw, ok := input.(json.RawMessage); ok {
		data = raw
	} else {
		var err error
		if data, err = json.Marshal(input); err != nil {
			return map[string]any{}
		}
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return map[string]any{}
	}
	return result
}

// extractTextFromContent 用换行拼接 genai.Content 里的所有文本 part。
func extractTextFromContent(content *genai.Content) string {
	if content == nil {
		return ""
	}
	var texts []string
	for _, part := range content.Parts {
		if part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// thinkingBlockFromPart 把带签名的思考 Part 还原成 thinking / redacted_thinking
// 块。没有签名的思考无法通过 API 校验，返回 false 表示应当丢弃。
func thinkingBlockFromPart(part *genai.Part) (anthropic.ContentBlockParamUnion, bool) {
	if len(part.ThoughtSignature) == 0 {
		return anthropic.ContentBlockParamUnion{}, false
	}

	sig := string(part.ThoughtSignature)
	if data, ok := strings.CutPrefix(sig, redactedThinkingMarker); ok {
		return anthropic.NewRedactedThinkingBlock(data), true
	}
	if part.Text == "" {
		return anthropic.ContentBlockParamUnion{}, false
	}
	return anthropic.NewThinkingBlock(sig, part.Text), true
}

// sanitizeToolId 把非法的工具 ID（含 [a-zA-Z0-9_-] 之外的字符）换成基于 SHA256 的合法 ID。
func sanitizeToolId(id string) string {
	if anthropicToolIdPattern.MatchString(id) {
		return id
	}

	// 用 SHA256 从原 ID 生成一个合法 ID
	hash := sha256.Sum256([]byte(id))
	return "toolu_" + hex.EncodeToString(hash[:16])
}

// repairMessageHistory 移除落单的 tool_use 与 tool_result 块。
func repairMessageHistory(messages []anthropic.MessageParam) []anthropic.MessageParam {
	if len(messages) == 0 {
		return messages
	}

	result := make([]anthropic.MessageParam, 0, len(messages))

	for i := 0; i < len(messages); i++ {
		msg := messages[i]

		if msg.Role == anthropic.MessageParamRoleAssistant {
			toolUseIds := extractToolUseIds(msg)
			if len(toolUseIds) > 0 {
				if i+1 < len(messages) && messages[i+1].Role == anthropic.MessageParamRoleUser {
					toolUseSet := make(map[string]bool, len(toolUseIds))
					for _, id := range toolUseIds {
						toolUseSet[id] = true
					}

					matchedIds := make(map[string]bool)
					for _, id := range extractToolResultIds(messages[i+1]) {
						if toolUseSet[id] {
							matchedIds[id] = true
						}
					}

					filteredMsg := filterToolUse(msg, matchedIds)
					if hasContent(filteredMsg) {
						result = append(result, filteredMsg)
					}
					filteredResult := filterToolResult(messages[i+1], matchedIds)
					if hasContent(filteredResult) {
						result = append(result, filteredResult)
					}
					i++
					continue
				}

				msg = filterToolUse(msg, nil)
			}
		}

		if msg.Role == anthropic.MessageParamRoleUser {
			msg = filterToolResult(msg, nil)
		}
		if hasContent(msg) {
			result = append(result, msg)
		}
	}

	return result
}

// trimFinalAssistantWhitespace 把结尾的 assistant 消息里最后一个文本块做右侧
// 去空白。Anthropic 会拒绝最终 assistant 内容以空白结尾的请求（"final
// assistant content cannot end with trailing whitespace"）：预填充要从这些
// token 精确续写，结尾空格的分词是有歧义的。去空白后变空的块会被丢掉，
// 因为空文本块同样会被拒绝。
func trimFinalAssistantWhitespace(messages []anthropic.MessageParam) []anthropic.MessageParam {
	if len(messages) == 0 {
		return messages
	}
	last := &messages[len(messages)-1]
	if last.Role != anthropic.MessageParamRoleAssistant {
		return messages
	}

	for i := len(last.Content) - 1; i >= 0; i-- {
		text := last.Content[i].OfText
		if text == nil {
			continue
		}
		text.Text = strings.TrimRight(text.Text, " \t\n\r")
		if text.Text == "" {
			last.Content = append(last.Content[:i], last.Content[i+1:]...)
		}
		break
	}
	return messages
}

// extractToolUseIds 返回 assistant 消息里的所有 tool_use ID。
func extractToolUseIds(msg anthropic.MessageParam) []string {
	var ids []string
	for _, block := range msg.Content {
		if block.OfToolUse != nil {
			ids = append(ids, block.OfToolUse.ID)
		}
	}
	return ids
}

// extractToolResultIds 返回 user 消息里的所有 tool_result ID。
func extractToolResultIds(msg anthropic.MessageParam) []string {
	var ids []string
	for _, block := range msg.Content {
		if block.OfToolResult != nil {
			ids = append(ids, block.OfToolResult.ToolUseID)
		}
	}
	return ids
}

// filterToolUse 只保留 ID 在 allowedIds 中的 tool_use 块。allowedIds 为 nil 时全部移除。
func filterToolUse(msg anthropic.MessageParam, allowedIds map[string]bool) anthropic.MessageParam {
	var filteredBlocks []anthropic.ContentBlockParamUnion
	for _, block := range msg.Content {
		if block.OfToolUse != nil {
			if allowedIds != nil && allowedIds[block.OfToolUse.ID] {
				filteredBlocks = append(filteredBlocks, block)
			}
			continue
		}
		filteredBlocks = append(filteredBlocks, block)
	}
	return anthropic.MessageParam{Role: msg.Role, Content: filteredBlocks}
}

// filterToolResult 只保留 ID 在 allowedIds 中的 tool_result 块。allowedIds 为 nil 时全部移除。
func filterToolResult(msg anthropic.MessageParam, allowedIds map[string]bool) anthropic.MessageParam {
	var filteredBlocks []anthropic.ContentBlockParamUnion
	for _, block := range msg.Content {
		if block.OfToolResult != nil {
			if allowedIds != nil && allowedIds[block.OfToolResult.ToolUseID] {
				filteredBlocks = append(filteredBlocks, block)
			}
			continue
		}
		filteredBlocks = append(filteredBlocks, block)
	}
	return anthropic.MessageParam{Role: msg.Role, Content: filteredBlocks}
}

// convertInlineDataToBlock 把内联数据转换成对应的 Anthropic 内容块。
// 支持图片（jpeg、png、gif、webp）、PDF 和纯文本文档。
// 遇到不支持的 MIME 类型返回错误，与 Gemini 的行为一致：让请求失败，
// 而不是悄悄丢掉内容。
func convertInlineDataToBlock(data *genai.Blob) (*anthropic.ContentBlockParamUnion, error) {
	if data == nil {
		return nil, fmt.Errorf("inline data is nil")
	}

	mediaType := data.MIMEType
	base64Data := base64.StdEncoding.EncodeToString(data.Data)

	switch {
	case mediaType == "image/jpeg" || mediaType == "image/jpg" || mediaType == "image/png" ||
		mediaType == "image/gif" || mediaType == "image/webp":
		return &anthropic.ContentBlockParamUnion{
			OfImage: &anthropic.ImageBlockParam{
				Source: anthropic.ImageBlockParamSourceUnion{
					OfBase64: &anthropic.Base64ImageSourceParam{
						MediaType: anthropic.Base64ImageSourceMediaType(mediaType),
						Data:      base64Data,
					},
				},
			},
		}, nil

	case mediaType == "application/pdf":
		return &anthropic.ContentBlockParamUnion{
			OfDocument: &anthropic.DocumentBlockParam{
				Source: anthropic.DocumentBlockParamSourceUnion{
					OfBase64: &anthropic.Base64PDFSourceParam{
						Data: base64Data,
					},
				},
			},
		}, nil

	case strings.HasPrefix(mediaType, "text/"):
		return &anthropic.ContentBlockParamUnion{
			OfDocument: &anthropic.DocumentBlockParam{
				Source: anthropic.DocumentBlockParamSourceUnion{
					OfText: &anthropic.PlainTextSourceParam{
						Data: string(data.Data),
					},
				},
			},
		}, nil

	default:
		return nil, fmt.Errorf("unsupported inline data MIME type for Anthropic: %s", mediaType)
	}
}

// hasContent 判断消息是否还有实质内容。只剩 thinking 块不算：思考描述的是同一
// 条消息里的文本或 tool_use，那些都被剔除（如孤立 tool_use 被
// repairMessageHistory 删掉）后，单独回传的思考轻则污染上下文，重则被 API
// 拒绝——thinking-only 的消息落在末尾还会撞上「扩展思考不允许 prefill」。
func hasContent(msg anthropic.MessageParam) bool {
	for _, block := range msg.Content {
		if block.OfThinking == nil && block.OfRedactedThinking == nil {
			return true
		}
	}
	return false
}

// lowercaseTypes 递归遍历 JSON schema map，把所有 "type" 字段转成小写，
// 以符合 Anthropic 的 JSON schema 校验要求。
func lowercaseTypes(m map[string]any) {
	for k, v := range m {
		if k == "type" {
			if s, ok := v.(string); ok {
				m[k] = strings.ToLower(s)
			}
		} else if vMap, ok := v.(map[string]any); ok {
			lowercaseTypes(vMap)
		} else if vList, ok := v.([]any); ok {
			for _, item := range vList {
				if itemMap, ok := item.(map[string]any); ok {
					lowercaseTypes(itemMap)
				}
			}
		}
	}
}
