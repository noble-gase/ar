package llmchat

import (
	"context"
	"fmt"
	"iter"

	"github.com/noble-gase/argon/session"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	adk_session "google.golang.org/adk/v2/session"
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
