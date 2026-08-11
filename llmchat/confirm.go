package llmchat

import (
	"encoding/json"
	"errors"

	"github.com/google/jsonschema-go/jsonschema"
	adk_session "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/adk/v2/workflow"
)

// Confirmation describes a pending Human-in-the-Loop tool confirmation request
// extracted from an event stream.
type Confirmation struct {
	// CallId is the ID that must be passed back to Confirm to resume execution.
	CallId string
	// ToolName is the tool the agent intends to run once approved.
	ToolName string
	// Args are the arguments the agent intends to call the tool with. May be nil.
	Args map[string]any
	// Hint is a human-readable prompt explaining why confirmation is needed and
	// what action is being confirmed.
	Hint string
	// Payload carries application-specific context attached to the confirmation
	// request. Its structure is defined by the tool. May be nil.
	Payload any
}

// ConfirmationOf returns the pending tool confirmation carried by an event, if
// any. It reports false when the event is not a confirmation request.
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

// RequestInput describes a pending human-input request raised by a graph
// workflow node (GraphAgent). Unlike Confirmation, which gates a tool call with
// approve/reject, it asks the user for a free-form answer.
type RequestInput struct {
	// InterruptId is the ID that must be passed back to Reply to resume the graph.
	InterruptId string
	// Message is the prompt shown to the user.
	Message string
	// Payload carries node-specific context attached to the request. Its
	// structure is defined by the node. May be nil.
	Payload any
	// ResponseSchema, when non-nil, is the schema the answer passed to Reply must
	// conform to. Use ReplyPayload to turn a text answer into a conforming value.
	ResponseSchema *jsonschema.Schema
}

// RequestInputOf returns the pending human-input request carried by an event, if
// any. It reports false when the event is not an input request.
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

// ReplyPayload converts a free-form text answer into the value expected by the
// node: when the node asks for structured data the text is decoded as JSON,
// otherwise it is passed through verbatim. Text that should be JSON but fails to
// parse is passed through as-is, so the node's own schema validation reports it.
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

// IsRejectedReply reports whether the error means the answer passed to Reply did
// not match the node's response schema. The node stays parked, so the same
// question can be asked again.
func IsRejectedReply(err error) bool {
	return errors.Is(err, workflow.ErrInvalidResumeResponse)
}
