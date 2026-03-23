package llmchat

import (
	"context"
	"fmt"
	"iter"
	"time"

	"github.com/noble-gase/ar/session"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/runner"
	adk_session "google.golang.org/adk/session"
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
func (c *Chat) Ask(ctx context.Context, userId, text string) (iter.Seq2[*adk_session.Event, error], error) {
	uid := userId + "_" + time.Now().Format("20060102")

	sid, err := c.session.GetOrCreate(ctx, uid)
	if err != nil {
		return nil, err
	}

	return c.runner.Run(ctx, uid, sid,
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
