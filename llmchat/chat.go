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

// Ask sends a message using the current automatic conversation for userId.
// It is intended for channels such as DingTalk that do not manage conversation IDs.
func (c *Chat) Ask(ctx context.Context, userId, text string) (iter.Seq2[*adk_session.Event, error], error) {
	conversation, err := c.session.GetOrCreate(ctx, userId, 1)
	if err != nil {
		return nil, err
	}
	return c.run(ctx, userId, conversation.ID(), genai.NewContentFromText(text, genai.RoleUser)), nil
}

// Confirm resumes a confirmation in the current automatic conversation.
func (c *Chat) Confirm(ctx context.Context, userId, callId string, approved bool, payload any) (iter.Seq2[*adk_session.Event, error], error) {
	conversation, err := c.session.GetOrCreate(ctx, userId, 1)
	if err != nil {
		return nil, err
	}
	return c.confirm(ctx, userId, conversation.ID(), callId, approved, payload), nil
}

// NewConversation creates a new conversation and returns its generated ID.
func (c *Chat) NewConversation(ctx context.Context, userId string) (string, error) {
	conversation, err := c.session.Create(ctx, userId, "")
	if err != nil {
		return "", err
	}
	return conversation.ID(), nil
}

// CreateConversation creates a conversation with a caller-provided ID.
func (c *Chat) CreateConversation(ctx context.Context, userId, conversationId string) error {
	_, err := c.session.Create(ctx, userId, conversationId)
	return err
}

func (c *Chat) GetConversation(ctx context.Context, userId, conversationId string) (adk_session.Session, error) {
	return c.session.Get(ctx, userId, conversationId)
}

// GetConversationMeta returns the product-level metadata, including the title.
func (c *Chat) GetConversationMeta(ctx context.Context, userId, conversationId string) (*session.Conversation, error) {
	return c.session.GetMeta(ctx, userId, conversationId)
}

// RenameConversation sets the display title of an existing conversation.
func (c *Chat) RenameConversation(ctx context.Context, userId, conversationId, title string) error {
	return c.session.Rename(ctx, userId, conversationId, title)
}

func (c *Chat) ListConversations(ctx context.Context, userId, cursor string, limit int) (*session.ConversationPage, error) {
	return c.session.List(ctx, userId, cursor, limit)
}

func (c *Chat) DeleteConversation(ctx context.Context, userId, conversationId string) error {
	return c.session.Delete(ctx, userId, conversationId)
}

// AskConversation sends a message to an existing explicit conversation.
func (c *Chat) AskConversation(ctx context.Context, userId, conversationId, text string) (iter.Seq2[*adk_session.Event, error], error) {
	if err := c.session.Touch(ctx, userId, conversationId); err != nil {
		return nil, err
	}
	return c.run(ctx, userId, conversationId, genai.NewContentFromText(text, genai.RoleUser)), nil
}

// ConfirmConversation resumes a paused tool-confirmation run in an explicit conversation.
// userId and conversationId must be the values used in the original Ask call, and
// callId is the Confirmation.CallID surfaced by ConfirmationOf. payload is an
// optional application-specific value forwarded to the tool (may be nil).
func (c *Chat) ConfirmConversation(ctx context.Context, userId, conversationId, callId string, approved bool, payload any) (iter.Seq2[*adk_session.Event, error], error) {
	if err := c.session.Touch(ctx, userId, conversationId); err != nil {
		return nil, err
	}
	return c.confirm(ctx, userId, conversationId, callId, approved, payload), nil
}

func (c *Chat) run(ctx context.Context, userId, sessionId string, content *genai.Content) iter.Seq2[*adk_session.Event, error] {
	return c.runner.Run(
		ctx, userId, sessionId, content,
		agent.RunConfig{
			StreamingMode: agent.StreamingModeSSE,
		},
	)
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
