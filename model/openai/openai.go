package openai

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
	"strings"

	"github.com/noble-gase/argon/model/common"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

var ErrNoChoicesInResponse = errors.New("no choices in OpenAI response")

// OpenAI 对 tool_call_id 字段有 40 字符的长度限制。
const maxToolCallIdLength = 40

// openaiModel 用官方 OpenAI Go SDK 实现 model.LLM。
// 同时适用于 OpenAI API 及其兼容实现（Ollama、vLLM 等）。
type openaiModel struct {
	client *openai.Client
	name   string
}

// HTTPOptions 是 OpenAI 客户端的可选 HTTP 层配置。
type HTTPOptions struct {
	Client  *http.Client
	Headers http.Header
}

// Config 是创建 OpenAI Model 所需的配置。
type Config struct {
	// APIKey 用于鉴权。留空则回退到环境变量 OPENAI_API_KEY。
	APIKey string
	// BaseURL 是 API 地址，用于对接 OpenAI 兼容的服务。
	// 例如 Ollama 为 "http://localhost:11434/v1"。
	BaseURL string
	// ModelName 指定使用的模型（如 "gpt-4o"、"qwen3:8b"）。
	ModelName string
	// HTTPOptions 是可选的 HTTP 层覆盖配置（如附加请求头）。
	HTTPOptions HTTPOptions
}

// NewModel 返回基于 OpenAI API 的 [model.LLM]。
func NewModel(cfg Config) model.LLM {
	var opts []option.RequestOption

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

	client := openai.NewClient(opts...)

	return &openaiModel{
		client: &client,
		name:   cfg.ModelName,
	}
}

// Name 返回模型名称。
func (m *openaiModel) Name() string {
	return m.name
}

// GenerateContent 向 LLM 发起请求并返回响应。
// stream 为 true 时流式返回，为 false 时只返回一个完整响应。
func (m *openaiModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	if stream {
		return m.generateStream(ctx, req)
	}
	return m.generate(ctx, req)
}

// generate 发起非流式请求，只产出一个响应。
func (m *openaiModel) generate(ctx context.Context, req *model.LLMRequest) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		params, err := m.buildChatCompletionParams(req, false)
		if err != nil {
			yield(nil, err)
			return
		}

		resp, err := m.client.Chat.Completions.New(ctx, params)
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

// generateStream 发起流式请求，边到达边产出增量响应，
// 最后再产出一个聚合后的完整响应。
func (m *openaiModel) generateStream(ctx context.Context, req *model.LLMRequest) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		params, err := m.buildChatCompletionParams(req, true)
		if err != nil {
			yield(nil, err)
			return
		}

		stream := m.client.Chat.Completions.NewStreaming(ctx, params)
		defer stream.Close()

		acc := openai.ChatCompletionAccumulator{}

		// 分片到达即产出
		for stream.Next() {
			chunk := stream.Current()
			acc.AddChunk(chunk)

			if len(chunk.Choices) == 0 {
				continue
			}

			delta := chunk.Choices[0].Delta
			// reasoning_content 是 OpenAI 兼容服务（Kimi K2.6、DeepSeek-R1、
			// Qwen3-Thinking 等）用来流式输出隐藏思维链的非标准字段。官方
			// OpenAI schema 里没有它，所以只能从原始 JSON 信封里读，而不是
			// 从 Delta 的类型化字段读。细节见 extractReasoningContent。
			reasoning := extractReasoningContent(delta.RawJSON())

			if delta.Content == "" && reasoning == "" {
				continue
			}

			// 顺序有意义：推理 token 先于最终答案 token 产生，所以 Part 的
			// 顺序要镜像模型产出它们的时间顺序。下游消费者（如 ADK 的
			// llmagent）会遍历 parts 并按 Thought 过滤，把推理放在前面正好
			// 符合自然的对话记录顺序。
			var parts []*genai.Part
			if reasoning != "" {
				parts = append(parts, &genai.Part{Text: reasoning, Thought: true})
			}
			if delta.Content != "" {
				parts = append(parts, &genai.Part{Text: delta.Content})
			}

			llmResp := &model.LLMResponse{
				Content: &genai.Content{
					Role:  genai.RoleModel,
					Parts: parts,
				},
				Partial:      true,
				TurnComplete: false,
			}
			if !yield(llmResp, nil) {
				return
			}
		}

		if err := stream.Err(); err != nil {
			yield(nil, err)
			return
		}

		// 构造并产出最终的聚合响应
		yield(m.buildStreamFinalResponse(&acc), nil)
	}
}

// buildStreamFinalResponse 用流式累积的数据构造最终的 LLMResponse。
func (m *openaiModel) buildStreamFinalResponse(acc *openai.ChatCompletionAccumulator) *model.LLMResponse {
	content := &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{},
	}

	if len(acc.Choices) > 0 {
		choice := acc.Choices[0]

		// 与 generateStream 中同理：openai-go 没有为这个非标准字段定型，
		// 只能从原始 JSON 读 reasoning_content。推理 Part 放在最终答案
		// Part 之前，以保留模型产出 token 的时间顺序。
		if reasoning := extractReasoningContent(choice.Message.RawJSON()); reasoning != "" {
			content.Parts = append(content.Parts, &genai.Part{Text: reasoning, Thought: true})
		}

		if choice.Message.Content != "" {
			content.Parts = append(content.Parts, &genai.Part{Text: choice.Message.Content})
		}

		for _, tc := range choice.Message.ToolCalls {
			content.Parts = append(content.Parts, &genai.Part{
				FunctionCall: &genai.FunctionCall{
					ID:   tc.ID,
					Name: tc.Function.Name,
					Args: parseJSONArgs(tc.Function.Arguments),
				},
			})
		}
	}

	var finishReason genai.FinishReason
	if len(acc.Choices) > 0 {
		finishReason = convertFinishReason(string(acc.Choices[0].FinishReason))
	}

	return &model.LLMResponse{
		Content:       content,
		UsageMetadata: convertUsageMetadata(acc.Usage),
		FinishReason:  finishReason,
		Partial:       false,
		TurnComplete:  true,
	}
}

// buildChatCompletionParams 把 LLMRequest 转换成 OpenAI 的 API 参数。
func (m *openaiModel) buildChatCompletionParams(req *model.LLMRequest, stream bool) (openai.ChatCompletionNewParams, error) {
	var messages []openai.ChatCompletionMessageParamUnion

	// 加入系统指令
	if req.Config != nil && req.Config.SystemInstruction != nil {
		if text := extractText(req.Config.SystemInstruction); text != "" {
			messages = append(messages, openai.SystemMessage(text))
		}
	}

	// 转换对话消息
	for _, content := range req.Contents {
		msgs, err := m.convertContentToMessages(content)
		if err != nil {
			return openai.ChatCompletionNewParams{}, err
		}
		messages = append(messages, msgs...)
	}

	params := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(m.name),
		Messages: messages,
	}
	if stream {
		params.StreamOptions = openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		}
	}

	// 应用可选配置
	if req.Config != nil {
		m.applyGenerationConfig(&params, req.Config)
	}

	return params, nil
}

// applyGenerationConfig 把可选的生成参数应用到请求上。
func (m *openaiModel) applyGenerationConfig(params *openai.ChatCompletionNewParams, cfg *genai.GenerateContentConfig) {
	if cfg.Temperature != nil {
		params.Temperature = openai.Float(float64(*cfg.Temperature))
	}
	if cfg.MaxOutputTokens > 0 {
		params.MaxTokens = openai.Int(int64(cfg.MaxOutputTokens))
	}
	if cfg.TopP != nil {
		params.TopP = openai.Float(float64(*cfg.TopP))
	}

	// 停止序列
	if len(cfg.StopSequences) == 1 {
		params.Stop = openai.ChatCompletionNewParamsStopUnion{
			OfString: openai.String(cfg.StopSequences[0]),
		}
	} else if len(cfg.StopSequences) > 1 {
		params.Stop = openai.ChatCompletionNewParamsStopUnion{
			OfStringArray: cfg.StopSequences,
		}
	}

	// 推理强度（o 系列模型）
	if cfg.ThinkingConfig != nil {
		params.ReasoningEffort = convertThinkingLevel(cfg.ThinkingConfig.ThinkingLevel)
	}

	// JSON 模式
	if cfg.ResponseMIMEType == "application/json" {
		params.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &openai.ResponseFormatJSONObjectParam{},
		}
	}

	// 带 schema 的结构化输出
	if cfg.ResponseSchema != nil {
		if schemaMap, err := convertSchema(cfg.ResponseSchema); err == nil {
			params.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
				OfJSONSchema: &openai.ResponseFormatJSONSchemaParam{
					JSONSchema: openai.ResponseFormatJSONSchemaJSONSchemaParam{
						Name:        "response",
						Description: openai.String(cfg.ResponseSchema.Description),
						Schema:      schemaMap,
						Strict:      openai.Bool(true),
					},
				},
			}
		}
	}

	// 工具
	if len(cfg.Tools) > 0 {
		if tools, err := m.convertTools(cfg.Tools); err == nil {
			params.Tools = tools
		}
	}

	// ToolConfig → tool_choice
	//
	// 把 genai.FunctionCallingConfig.Mode 映射到 OpenAI 的 tool_choice：
	//   ModeAuto → "auto"     （默认行为，模型可调可不调工具）
	//   ModeAny  → "required" （模型必须调工具，用于无法处理纯文本回复的
	//                          agent 循环）
	//   ModeNone → "none"     （本次调用禁用工具，即使传了工具定义）
	//
	// ModeAny 同时设置了 AllowedFunctionNames 时，OpenAI 的对应物是「指定
	// 某个函数」——这里取第一个名字，因为 OpenAI 的 tool_choice 只接受单个
	// 函数而不是列表。需要多函数白名单的调用方，应当用 ModeAny 加提示词
	// 来约束模型在允许的集合内选择。
	if cfg.ToolConfig != nil && cfg.ToolConfig.FunctionCallingConfig != nil {
		fcc := cfg.ToolConfig.FunctionCallingConfig
		switch fcc.Mode {
		case genai.FunctionCallingConfigModeAuto:
			params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: openai.String("auto"),
			}
		case genai.FunctionCallingConfigModeNone:
			params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: openai.String("none"),
			}
		case genai.FunctionCallingConfigModeAny:
			if len(fcc.AllowedFunctionNames) == 1 {
				params.ToolChoice = openai.ToolChoiceOptionFunctionToolChoice(
					openai.ChatCompletionNamedToolChoiceFunctionParam{
						Name: fcc.AllowedFunctionNames[0],
					},
				)
			} else {
				params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
					OfAuto: openai.String("required"),
				}
			}
		}
	}
}

// convertContentToMessages 把 genai.Content 转换成 OpenAI 的消息格式。
// 支持文本、图片、音频、文件、函数调用与函数响应。
func (m *openaiModel) convertContentToMessages(content *genai.Content) ([]openai.ChatCompletionMessageParamUnion, error) {
	var messages []openai.ChatCompletionMessageParamUnion

	var textParts []string
	var toolCalls []openai.ChatCompletionMessageToolCallUnionParam
	var mediaParts []openai.ChatCompletionContentPartUnionParam

	for _, part := range content.Parts {
		switch {
		case part.FunctionResponse != nil:
			responseJSON, err := common.MarshalPayload(part.FunctionResponse.Response)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal function response: %w", err)
			}
			normalizedId := m.normalizeToolCallId(part.FunctionResponse.ID)
			messages = append(messages, openai.ToolMessage(string(responseJSON), normalizedId))
		case part.FunctionCall != nil:
			argsJSON, err := common.MarshalPayload(part.FunctionCall.Args)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal function args: %w", err)
			}
			normalizedId := m.normalizeToolCallId(part.FunctionCall.ID)
			toolCalls = append(toolCalls, openai.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID: normalizedId,
					Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name:      part.FunctionCall.Name,
						Arguments: string(argsJSON),
					},
				},
			})

		case part.Text != "":
			textParts = append(textParts, part.Text)
		case part.InlineData != nil:
			p, err := convertInlineDataToPart(part.InlineData)
			if err != nil {
				return nil, err
			}
			mediaParts = append(mediaParts, *p)
		}
	}

	if len(textParts) > 0 || len(mediaParts) > 0 || len(toolCalls) > 0 {
		msg := m.buildRoleMessage(content.Role, textParts, mediaParts, toolCalls)
		if msg != nil {
			messages = append(messages, *msg)
		}
	}

	return messages, nil
}

// buildRoleMessage 按 role 构造对应类型的消息。
func (m *openaiModel) buildRoleMessage(role string, texts []string, media []openai.ChatCompletionContentPartUnionParam, toolCalls []openai.ChatCompletionMessageToolCallUnionParam) *openai.ChatCompletionMessageParamUnion {
	switch convertRole(role) {
	case "user":
		return buildUserMessage(texts, media)
	case "assistant":
		return buildAssistantMessage(texts, toolCalls)
	case "system":
		msg := openai.SystemMessage(joinTexts(texts))
		return &msg
	}
	return nil
}

// buildUserMessage 构造用户消息，支持多媒体的多 part 形式。
func buildUserMessage(texts []string, media []openai.ChatCompletionContentPartUnionParam) *openai.ChatCompletionMessageParamUnion {
	if len(media) == 0 {
		msg := openai.UserMessage(joinTexts(texts))
		return &msg
	}

	var parts []openai.ChatCompletionContentPartUnionParam
	for _, text := range texts {
		parts = append(parts, openai.ChatCompletionContentPartUnionParam{
			OfText: &openai.ChatCompletionContentPartTextParam{Text: text},
		})
	}
	parts = append(parts, media...)

	return &openai.ChatCompletionMessageParamUnion{
		OfUser: &openai.ChatCompletionUserMessageParam{
			Content: openai.ChatCompletionUserMessageParamContentUnion{
				OfArrayOfContentParts: parts,
			},
		},
	}
}

// buildAssistantMessage 构造助手消息，可带工具调用。
func buildAssistantMessage(texts []string, toolCalls []openai.ChatCompletionMessageToolCallUnionParam) *openai.ChatCompletionMessageParamUnion {
	msg := openai.ChatCompletionAssistantMessageParam{}

	if len(texts) > 0 {
		msg.Content = openai.ChatCompletionAssistantMessageParamContentUnion{
			OfString: openai.String(joinTexts(texts)),
		}
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}

	return &openai.ChatCompletionMessageParamUnion{OfAssistant: &msg}
}

// convertResponse 把 OpenAI 的响应转换成 LLMResponse。
func (m *openaiModel) convertResponse(resp *openai.ChatCompletion) (*model.LLMResponse, error) {
	if len(resp.Choices) == 0 {
		return nil, ErrNoChoicesInResponse
	}

	choice := resp.Choices[0]
	content := &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{},
	}

	// 与 buildStreamFinalResponse 中同理：openai-go 没有为 OpenAI 兼容的
	// 推理服务所用的这个非标准字段定型，只能从原始 JSON 读
	// reasoning_content。
	if reasoning := extractReasoningContent(choice.Message.RawJSON()); reasoning != "" {
		content.Parts = append(content.Parts, &genai.Part{Text: reasoning, Thought: true})
	}

	if choice.Message.Content != "" {
		content.Parts = append(content.Parts, &genai.Part{Text: choice.Message.Content})
	}

	for _, tc := range choice.Message.ToolCalls {
		content.Parts = append(content.Parts, &genai.Part{
			FunctionCall: &genai.FunctionCall{
				ID:   tc.ID,
				Name: tc.Function.Name,
				Args: parseJSONArgs(tc.Function.Arguments),
			},
		})
	}

	return &model.LLMResponse{
		Content:       content,
		UsageMetadata: convertUsageMetadata(resp.Usage),
		FinishReason:  convertFinishReason(string(choice.FinishReason)),
		TurnComplete:  true,
	}, nil
}

// convertTools 把 genai 的工具定义转换成 OpenAI 的函数工具格式。
func (m *openaiModel) convertTools(genaiTools []*genai.Tool) ([]openai.ChatCompletionToolUnionParam, error) {
	var tools []openai.ChatCompletionToolUnionParam

	for _, genaiTool := range genaiTools {
		if genaiTool == nil {
			continue
		}

		for _, funcDecl := range genaiTool.FunctionDeclarations {
			params := funcDecl.ParametersJsonSchema
			if params == nil {
				params = funcDecl.Parameters
			}

			tools = append(tools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
				Name:        funcDecl.Name,
				Description: openai.String(funcDecl.Description),
				Parameters:  convertToFunctionParams(params),
			}))
		}
	}

	return tools, nil
}

// convertToFunctionParams 把各种参数类型转换成 OpenAI 的格式。
// OpenAI 要求 object 类型的 schema 必须有 "properties" 字段，哪怕是空的。
func convertToFunctionParams(params any) shared.FunctionParameters {
	if params == nil {
		return nil
	}

	var m map[string]any

	// 直接就是 map
	if dm, ok := params.(map[string]any); ok {
		m = dm
	} else {
		// 其他类型（如 *jsonschema.Schema）走 JSON 转换
		jsonBytes, err := json.Marshal(params)
		if err != nil {
			return nil
		}
		if json.Unmarshal(jsonBytes, &m) != nil {
			return nil
		}
	}

	// 类型统一转小写，符合 JSON schema 规范
	lowercaseTypes(m)
	// OpenAI 要求 object 类型必须有 "properties"
	ensureObjectProperties(m)

	return shared.FunctionParameters(m)
}

// ensureObjectProperties 递归确保所有 object schema 都有 properties 字段。
func ensureObjectProperties(schema map[string]any) {
	if schema == nil {
		return
	}

	// type 为 "object" 且没有 properties 时补一个空的
	if t, ok := schema["type"].(string); ok && t == "object" {
		if _, hasProps := schema["properties"]; !hasProps {
			schema["properties"] = map[string]any{}
		}
	}

	// 递归处理嵌套的 properties
	if props, ok := schema["properties"].(map[string]any); ok {
		for _, prop := range props {
			if propMap, ok := prop.(map[string]any); ok {
				ensureObjectProperties(propMap)
			}
		}
	}

	// 处理数组元素
	if items, ok := schema["items"].(map[string]any); ok {
		ensureObjectProperties(items)
	}
}

// lowercaseTypes 递归遍历 JSON schema map，把所有 "type" 字段转成小写，
// 以符合标准 JSON schema 的校验要求。
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

// convertSchema 递归把 genai.Schema 转换成 OpenAI 的 JSON schema 格式。
func convertSchema(schema *genai.Schema) (map[string]any, error) {
	if schema == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}, nil
	}

	result := make(map[string]any)

	if schema.Type != genai.TypeUnspecified {
		result["type"] = schemaTypeToString(schema.Type)
	}
	if schema.Description != "" {
		result["description"] = schema.Description
	}
	if len(schema.Required) > 0 {
		result["required"] = schema.Required
	}
	if len(schema.Enum) > 0 {
		result["enum"] = schema.Enum
	}

	if len(schema.Properties) > 0 {
		props := make(map[string]any)
		for name, propSchema := range schema.Properties {
			converted, err := convertSchema(propSchema)
			if err != nil {
				return nil, err
			}
			props[name] = converted
		}
		result["properties"] = props
	}

	if schema.Items != nil {
		items, err := convertSchema(schema.Items)
		if err != nil {
			return nil, err
		}
		result["items"] = items
	}

	return result, nil
}

// normalizeToolCallId 用哈希把超过 OpenAI 40 字符上限的 ID 缩短，
// 映射会被保存下来以便需要时反查。
func (m *openaiModel) normalizeToolCallId(id string) string {
	if len(id) <= maxToolCallIdLength {
		return id
	}

	hash := sha256.Sum256([]byte(id))
	shortId := "tc_" + hex.EncodeToString(hash[:])[:maxToolCallIdLength-3]
	return shortId
}

// --- 辅助函数 ---

// convertInlineDataToPart 把内联数据转换成对应的 OpenAI 内容 part。
// 支持图片（data URI）、音频（wav、mp3）和通用文件（PDF 等）。
// 遇到不支持的 MIME 类型返回错误，与 Gemini 的行为一致：让请求失败，
// 而不是悄悄丢掉内容。
func convertInlineDataToPart(data *genai.Blob) (*openai.ChatCompletionContentPartUnionParam, error) {
	if data == nil {
		return nil, fmt.Errorf("inline data is nil")
	}

	mediaType := data.MIMEType
	base64Data := base64.StdEncoding.EncodeToString(data.Data)

	switch {
	case mediaType == "image/jpeg" || mediaType == "image/jpg" || mediaType == "image/png" ||
		mediaType == "image/gif" || mediaType == "image/webp":
		return &openai.ChatCompletionContentPartUnionParam{
			OfImageURL: &openai.ChatCompletionContentPartImageParam{
				ImageURL: openai.ChatCompletionContentPartImageImageURLParam{
					URL:    fmt.Sprintf("data:%s;base64,%s", mediaType, base64Data),
					Detail: "auto",
				},
			},
		}, nil

	case mediaType == "audio/wav" || mediaType == "audio/mp3" ||
		mediaType == "audio/mpeg" || mediaType == "audio/webm":
		format := "wav"
		if mediaType == "audio/mp3" || mediaType == "audio/mpeg" {
			format = "mp3"
		}
		return &openai.ChatCompletionContentPartUnionParam{
			OfInputAudio: &openai.ChatCompletionContentPartInputAudioParam{
				InputAudio: openai.ChatCompletionContentPartInputAudioInputAudioParam{
					Data:   base64Data,
					Format: format,
				},
			},
		}, nil

	case mediaType == "application/pdf" || strings.HasPrefix(mediaType, "text/"):
		return &openai.ChatCompletionContentPartUnionParam{
			OfFile: &openai.ChatCompletionContentPartFileParam{
				File: openai.ChatCompletionContentPartFileFileParam{
					FileData: openai.String(fmt.Sprintf("data:%s;base64,%s", mediaType, base64Data)),
				},
			},
		}, nil

	default:
		return nil, fmt.Errorf("unsupported inline data MIME type for OpenAI: %s", mediaType)
	}
}

// convertUsageMetadata 把 OpenAI 的用量统计转换成 genai 格式。
//
// CompletionTokensDetails.ReasoningTokens 是隐藏推理 token 的数量，
// OpenAI 的推理模型（o 系列、gpt-5.x）以及暴露推理过程的兼容服务
// （DeepSeek、Kimi K2/K2.6、Qwen3-Thinking）都按输出 token 计费。
// 它是官方 Chat Completions schema 的一部分，所以总是映射到 genai 的
// ThoughtsTokenCount，无论服务端是否同时返回推理文本。服务端不产生推理
// token 时该字段为零，genai 序列化会因 `omitempty` 省略它。
func convertUsageMetadata(usage openai.CompletionUsage) *genai.GenerateContentResponseUsageMetadata {
	if usage.TotalTokens == 0 {
		return nil
	}
	return &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     int32(usage.PromptTokens),
		CandidatesTokenCount: int32(usage.CompletionTokens),
		TotalTokenCount:      int32(usage.TotalTokens),
		ThoughtsTokenCount:   int32(usage.CompletionTokensDetails.ReasoningTokens),
	}
}

// extractReasoningContent 从 SDK 的原始 JSON 信封里读取非标准的
// "reasoning_content" 字段。
//
// OpenAI 的 Chat Completions schema **没有** "reasoning_content" 字段——对
// OpenAI 自家的推理模型（o 系列、gpt-5.x）来说，推理文本是隐藏的，只上报
// token 数量（通过 CompletionTokensDetails.ReasoningTokens）。推理**文本**
// 只能通过 Responses API 拿到，而本适配器不用那个 API。
//
// 但不少 OpenAI 兼容服务（DeepSeek-R1、Kimi K2/K2.6、Qwen3-Thinking 等）会在
// choices[].message 和 choices[].delta 上扩展出 "reasoning_content" 字段。
// openai-go 没有为它定型，但会原样保留在 JSON.raw 里，可以通过生成的
// RawJSON() 访问。解析原始信封正是这个 SDK 读取非标准字段的官方做法。
//
// 字段缺失、为空或 JSON 无法解析时返回 ""——调用方应当把空值当作「没有产出
// 推理内容」，跳过添加 thought Part。
func extractReasoningContent(rawJSON string) string {
	if rawJSON == "" {
		return ""
	}
	var probe struct {
		ReasoningContent string `json:"reasoning_content"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &probe); err != nil {
		return ""
	}
	return probe.ReasoningContent
}

// convertRole 把 genai 的 role 映射到 OpenAI 的 role。
func convertRole(role string) string {
	if role == "model" {
		return "assistant"
	}
	return role // "user" 和 "system" 两边一致
}

// convertFinishReason 把 OpenAI 的结束原因映射成 genai 格式。
func convertFinishReason(reason string) genai.FinishReason {
	switch reason {
	case "stop", "tool_calls", "function_call":
		return genai.FinishReasonStop
	case "length":
		return genai.FinishReasonMaxTokens
	case "content_filter":
		return genai.FinishReasonSafety
	default:
		return genai.FinishReasonUnspecified
	}
}

// convertThinkingLevel 把 genai 的思考档位映射成 OpenAI 的 reasoning effort。
func convertThinkingLevel(level genai.ThinkingLevel) shared.ReasoningEffort {
	switch level {
	case genai.ThinkingLevelLow:
		return shared.ReasoningEffortLow
	case genai.ThinkingLevelHigh:
		return shared.ReasoningEffortHigh
	default:
		return shared.ReasoningEffortMedium
	}
}

// schemaTypeToString 把 genai.Type 转换成 JSON schema 的类型字符串。
func schemaTypeToString(t genai.Type) string {
	types := map[genai.Type]string{
		genai.TypeString:  "string",
		genai.TypeNumber:  "number",
		genai.TypeInteger: "integer",
		genai.TypeBoolean: "boolean",
		genai.TypeArray:   "array",
		genai.TypeObject:  "object",
	}
	if s, ok := types[t]; ok {
		return s
	}
	return "string"
}

// extractText 取出 Content 里的所有文本 part 并拼接。
func extractText(content *genai.Content) string {
	if content == nil {
		return ""
	}
	var texts []string
	for _, part := range content.Parts {
		if part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return joinTexts(texts)
}

// joinTexts 用换行拼接多段文本。
func joinTexts(texts []string) string {
	return strings.Join(texts, "\n")
}

// parseJSONArgs 把 JSON 字符串解析成 map，出错时返回空 map。
func parseJSONArgs(argsJSON string) map[string]any {
	if argsJSON == "" {
		return make(map[string]any)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return make(map[string]any)
	}
	return args
}
