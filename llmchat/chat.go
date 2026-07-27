package llmchat

import (
	"context"
	"fmt"
	"iter"

	"github.com/noble-gase/argon/session"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	adk_session "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

type Chat struct {
	runner  *runner.Runner
	session *session.Session
}

func (c *Chat) Name() string {
	return c.session.AppName()
}

// Ask 问答
func (c *Chat) Ask(ctx context.Context, conversationId, text string) (iter.Seq2[*adk_session.Event, error], error) {
	sessionId, err := c.session.GetOrCreate(ctx, conversationId)
	if err != nil {
		return nil, err
	}

	return c.runner.Run(
		ctx, conversationId, sessionId,
		genai.NewContentFromText(text, genai.RoleUser),
		agent.RunConfig{
			StreamingMode: agent.StreamingModeSSE,
		},
	), nil
}

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

// Confirm resumes a paused tool-confirmation run with the user's decision.
// conversationId must be the same value used in the original Ask call, and
// callID is the Confirmation.CallID surfaced by ConfirmationOf. payload is an
// optional application-specific value forwarded to the tool (may be nil).
func (c *Chat) Confirm(ctx context.Context, conversationId, callId string, approved bool, payload any) (iter.Seq2[*adk_session.Event, error], error) {
	sid, err := c.session.GetOrCreate(ctx, conversationId)
	if err != nil {
		return nil, err
	}

	response := map[string]any{
		"confirmed": approved,
	}
	if payload != nil {
		response["payload"] = payload
	}

	content := &genai.Content{
		Role: string(genai.RoleUser),
		Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				Name:     toolconfirmation.FunctionCallName,
				ID:       callId,
				Response: response,
			},
		}},
	}

	return c.runner.Run(
		ctx, conversationId, sid, content,
		agent.RunConfig{
			StreamingMode: agent.StreamingModeSSE,
		},
	), nil
}

func NewChat(agent agent.Agent, session *session.Session) (*Chat, error) {
	// Runner
	r, err := runner.New(runner.Config{
		AppName:        session.AppName(),
		Agent:          agent,
		SessionService: session.Service(),
	})
	if err != nil {
		return nil, err
	}

	fmt.Println("ADK llmchat success")

	return &Chat{
		runner:  r,
		session: session,
	}, nil
}
