package llmchat

import (
	adk_session "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
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
