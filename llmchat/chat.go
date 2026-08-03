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

func (c *Chat) AutoModeEnabled() bool {
	return c.session.AutoModeEnabled()
}

func (c *Chat) Close() error {
	return c.session.Close()
}

// NewConversation creates a new conversation and returns its generated ID.
func (c *Chat) NewConversation(ctx context.Context, userId string) (string, error) {
	conversation, err := c.session.CreateConversation(ctx, userId, "")
	if err != nil {
		return "", err
	}
	return conversation.ID(), nil
}

// CreateConversation creates a conversation with a caller-provided ID.
func (c *Chat) CreateConversation(ctx context.Context, userId, conversationId string) error {
	_, err := c.session.CreateConversation(ctx, userId, conversationId)
	return err
}

func (c *Chat) GetConversation(ctx context.Context, userId, conversationId string) (adk_session.Session, error) {
	return c.session.GetConversation(ctx, userId, conversationId)
}

func (c *Chat) ListConversations(ctx context.Context, userId, cursor string, limit int) (*session.ConversationPage, error) {
	return c.session.ListConversations(ctx, userId, cursor, limit)
}

func (c *Chat) DeleteConversation(ctx context.Context, userId, conversationId string) error {
	return c.session.DeleteConversation(ctx, userId, conversationId)
}

// Ask sends a message to an existing explicit conversation.
func (c *Chat) Ask(ctx context.Context, userId, conversationId, text string) (iter.Seq2[*adk_session.Event, error], error) {
	if err := c.session.TouchConversation(ctx, userId, conversationId); err != nil {
		return nil, err
	}
	return c.run(ctx, userId, conversationId, genai.NewContentFromText(text, genai.RoleUser)), nil
}

// AskAuto sends a message using the current automatic conversation for userId.
// It is intended for channels such as DingTalk that do not manage conversation IDs.
func (c *Chat) AskAuto(ctx context.Context, userId, text string) (iter.Seq2[*adk_session.Event, error], error) {
	sessionId, err := c.session.GetOrCreate(ctx, userId)
	if err != nil {
		return nil, err
	}
	return c.run(ctx, userId, sessionId, genai.NewContentFromText(text, genai.RoleUser)), nil
}

func (c *Chat) run(ctx context.Context, userId, sessionId string, content *genai.Content) iter.Seq2[*adk_session.Event, error] {
	return c.runner.Run(
		ctx, userId, sessionId, content,
		agent.RunConfig{
			StreamingMode: agent.StreamingModeSSE,
		},
	)
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

// Confirm resumes a paused tool-confirmation run in an explicit conversation.
// userId and conversationId must be the values used in the original Ask call, and
// callId is the Confirmation.CallID surfaced by ConfirmationOf. payload is an
// optional application-specific value forwarded to the tool (may be nil).
func (c *Chat) Confirm(ctx context.Context, userId, conversationId, callId string, approved bool, payload any) (iter.Seq2[*adk_session.Event, error], error) {
	if err := c.session.TouchConversation(ctx, userId, conversationId); err != nil {
		return nil, err
	}
	return c.confirm(ctx, userId, conversationId, callId, approved, payload), nil
}

// ConfirmAuto resumes a confirmation in the current automatic conversation.
func (c *Chat) ConfirmAuto(ctx context.Context, userId, callId string, approved bool, payload any) (iter.Seq2[*adk_session.Event, error], error) {
	sessionId, err := c.session.GetOrCreate(ctx, userId)
	if err != nil {
		return nil, err
	}
	return c.confirm(ctx, userId, sessionId, callId, approved, payload), nil
}

func (c *Chat) confirm(ctx context.Context, userId, sessionId, callId string, approved bool, payload any) iter.Seq2[*adk_session.Event, error] {
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

	return c.run(ctx, userId, sessionId, content)
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
