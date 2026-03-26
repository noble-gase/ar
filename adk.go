package ar

import (
	"github.com/noble-gase/ar/dingtalk"
	"github.com/noble-gase/ar/llmchat"
	"github.com/noble-gase/ar/session"
	"github.com/redis/go-redis/v9"
	"google.golang.org/adk/agent"
	"gorm.io/gorm"
)

// NewLLMAgent returns a LLM agent.
func NewLLMAgent(builder llmchat.AgentBuilder) (agent.Agent, error) {
	return builder.Build(nil)
}

// NewLLMChat returns a LLM chat.
func NewLLMChat(name string, db gorm.Dialector, uc redis.UniversalClient, ab llmchat.AgentBuilder) (*llmchat.Chat, error) {
	// Session
	session, err := session.New(name, db, uc)
	if err != nil {
		return nil, err
	}

	// Agent
	agent, err := ab.Build(nil)
	if err != nil {
		return nil, err
	}

	// Chat
	chat, err := llmchat.NewChat(agent, session)
	if err != nil {
		return nil, err
	}
	return chat, nil
}

type Assistant struct {
	bot *dingtalk.Bot
}

func (a *Assistant) Start() {
	a.bot.Start()
}

func (a *Assistant) Stop() {
	a.bot.Stop()
}

// NewAssistant returns a DingTalk assistant.
func NewAssistant(clientId, clientSecret, cardTemplateId string, uc redis.UniversalClient, chat *llmchat.Chat) (*Assistant, error) {
	card, err := dingtalk.NewCardSender(clientId, clientSecret, cardTemplateId, uc)
	if err != nil {
		return nil, err
	}

	bot := dingtalk.NewBot(clientId, clientSecret, chat, card)
	return &Assistant{bot: bot}, nil
}
