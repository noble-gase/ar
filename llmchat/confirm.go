package llmchat

import (
	"encoding/json"
	"errors"

	"github.com/google/jsonschema-go/jsonschema"
	adk_session "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/adk/v2/workflow"
)

// Confirmation 描述从事件流里提取出的、待人工确认的工具调用请求。
type Confirmation struct {
	// CallId 是恢复执行时必须传回给 Confirm 的 ID。
	CallId string
	// ToolName 是获批后 agent 打算执行的工具。
	ToolName string
	// Args 是 agent 打算传给工具的参数，可能为 nil。
	Args map[string]any
	// Hint 是给人看的说明，解释为什么需要确认、正在确认什么动作。
	Hint string
	// Payload 携带附在确认请求上的应用自定义上下文，结构由工具决定，可能为 nil。
	Payload any
}

// ConfirmationOf 返回事件里携带的待确认工具调用。事件不是确认请求时返回 false。
func ConfirmationOf(event *adk_session.Event) (*Confirmation, bool) {
	if event == nil || event.Content == nil {
		return nil, false
	}
	for _, part := range event.Content.Parts {
		fc := part.FunctionCall
		if fc == nil || fc.Name != toolconfirmation.FunctionCallName {
			continue
		}
		c := &Confirmation{CallId: fc.ID}
		if orig, err := toolconfirmation.OriginalCallFrom(fc); err == nil {
			c.ToolName = orig.Name
			c.Args = orig.Args
		}
		if tc, ok := fc.Args["toolConfirmation"].(map[string]any); ok {
			if hint, ok := tc["hint"].(string); ok {
				c.Hint = hint
			}
			if payload, ok := tc["payload"]; ok {
				c.Payload = payload
			}
		}
		return c, true
	}
	return nil, false
}

// RequestInput 描述图工作流节点（GraphAgent）发起的、待人工输入的请求。
// 与 Confirmation 用同意/拒绝拦截工具调用不同，它是向用户要一段自由文本回答。
type RequestInput struct {
	// InterruptId 是恢复图执行时必须传回给 Reply 的 ID。
	InterruptId string
	// Message 是展示给用户的提问。
	Message string
	// Payload 携带附在请求上的节点自定义上下文，结构由节点决定，可能为 nil。
	Payload any
	// ResponseSchema 非 nil 时，传给 Reply 的回答必须符合这个 schema。
	// 用 ReplyPayload 可以把文本回答转成符合要求的值。
	ResponseSchema *jsonschema.Schema
}

// RequestInputOf 返回事件里携带的待人工输入请求。事件不是输入请求时返回 false。
func RequestInputOf(event *adk_session.Event) (*RequestInput, bool) {
	if event == nil {
		return nil, false
	}

	if req := event.RequestedInput; req != nil {
		return &RequestInput{
			InterruptId:    req.InterruptID,
			Message:        req.Message,
			Payload:        req.Payload,
			ResponseSchema: req.ResponseSchema,
		}, true
	}

	// 事件从会话历史反序列化回来时 RequestedInput 可能已丢失，回退到合成的 FunctionCall
	if event.Content == nil {
		return nil, false
	}
	for _, part := range event.Content.Parts {
		fc := part.FunctionCall
		if fc == nil || fc.Name != workflow.WorkflowInputFunctionCallName {
			continue
		}
		r := &RequestInput{InterruptId: fc.ID}
		if msg, ok := fc.Args["message"].(string); ok {
			r.Message = msg
		}
		r.Payload = fc.Args["payload"]
		r.ResponseSchema = decodeSchema(fc.Args["responseSchema"])
		return r, true
	}
	return nil, false
}

// decodeSchema 还原 FunctionCall 参数里的 schema，它在序列化往返后是普通 map。
func decodeSchema(v any) *jsonschema.Schema {
	if v == nil {
		return nil
	}
	if s, ok := v.(*jsonschema.Schema); ok {
		return s
	}

	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		return nil
	}
	return &s
}

// ReplyPayload 把自由文本回答转换成节点期望的值：节点要结构化数据时，文本会
// 按 JSON 解码，否则原样传递。本该是 JSON 却解析失败的文本会原样传过去，
// 交由节点自己的 schema 校验来报错。
func ReplyPayload(text string, schema *jsonschema.Schema) any {
	if schema == nil || schema.Type == "" || schema.Type == "string" {
		return text
	}

	var decoded any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		return text
	}
	return decoded
}

// IsRejectedReply 判断错误是否表示传给 Reply 的回答不符合节点的响应 schema。
// 此时节点仍处于 parked 状态，同一个问题可以再问一次。
func IsRejectedReply(err error) bool {
	return errors.Is(err, workflow.ErrInvalidResumeResponse)
}
