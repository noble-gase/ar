package llmchat

import (
	"testing"

	adk_session "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

func eventWithFunctionCall(fc *genai.FunctionCall) *adk_session.Event {
	ev := &adk_session.Event{}
	ev.Content = &genai.Content{
		Role:  string(genai.RoleModel),
		Parts: []*genai.Part{{FunctionCall: fc}},
	}
	return ev
}

func TestRequestInputOfFromRequestedInput(t *testing.T) {
	ev := &adk_session.Event{}
	ev.RequestedInput = &adk_session.RequestInput{
		InterruptID: "review-1",
		Message:     "请确认初稿",
		Payload:     map[string]any{"draft": "hello"},
	}

	got, ok := RequestInputOf(ev)
	if !ok {
		t.Fatal("RequestInputOf() ok = false, want true")
	}
	if got.InterruptId != "review-1" || got.Message != "请确认初稿" {
		t.Errorf("RequestInputOf() = %+v, want interruptId/message from RequestedInput", got)
	}
	if got.Payload == nil {
		t.Error("RequestInputOf() Payload = nil, want the request payload")
	}
}

// 事件从会话历史反序列化回来时 RequestedInput 可能丢失，此时必须能从合成的
// FunctionCall 里还原。
func TestRequestInputOfFromFunctionCall(t *testing.T) {
	ev := eventWithFunctionCall(&genai.FunctionCall{
		ID:   "review-2",
		Name: workflow.WorkflowInputFunctionCallName,
		Args: map[string]any{
			"interruptId": "review-2",
			"message":     "请补充预算",
			"payload":     "ctx",
		},
	})

	got, ok := RequestInputOf(ev)
	if !ok {
		t.Fatal("RequestInputOf() ok = false, want true")
	}
	if got.InterruptId != "review-2" {
		t.Errorf("InterruptId = %q, want %q", got.InterruptId, "review-2")
	}
	if got.Message != "请补充预算" {
		t.Errorf("Message = %q, want %q", got.Message, "请补充预算")
	}
	if got.Payload != "ctx" {
		t.Errorf("Payload = %v, want %q", got.Payload, "ctx")
	}
}

func TestRequestInputOfIgnoresOtherEvents(t *testing.T) {
	textEvent := &adk_session.Event{}
	textEvent.Content = genai.NewContentFromText("hi", genai.RoleModel)

	tests := map[string]*adk_session.Event{
		"nil event":    nil,
		"empty event":  {},
		"text event":   textEvent,
		"confirmation": eventWithFunctionCall(&genai.FunctionCall{ID: "c1", Name: toolconfirmation.FunctionCallName}),
	}

	for name, ev := range tests {
		t.Run(name, func(t *testing.T) {
			if _, ok := RequestInputOf(ev); ok {
				t.Error("RequestInputOf() ok = true, want false")
			}
		})
	}
}

func TestConfirmationOfIgnoresRequestInput(t *testing.T) {
	ev := eventWithFunctionCall(&genai.FunctionCall{
		ID:   "review-3",
		Name: workflow.WorkflowInputFunctionCallName,
	})

	if _, ok := ConfirmationOf(ev); ok {
		t.Error("ConfirmationOf() ok = true, want false for a human-input request")
	}
}
