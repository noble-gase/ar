package ar

import (
	"github.com/noble-gase/ar/dingtalk"
	"github.com/noble-gase/ar/llmchat"
	"github.com/noble-gase/ar/session"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

func NewNormalChat(name string, db gorm.Dialector, uc redis.UniversalClient, cfg *llmchat.NormalConfig) (*llmchat.Chat, error) {
	// Session
	session, err := session.New(name, db, uc)
	if err != nil {
		return nil, err
	}

	// Agent
	agent, err := llmchat.NewNormalAgent(cfg)
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

func NewAgentToolChat(name string, db gorm.Dialector, uc redis.UniversalClient, cfg *llmchat.AgentToolConfig) (*llmchat.Chat, error) {
	// Session
	session, err := session.New(name, db, uc)
	if err != nil {
		return nil, err
	}

	// Agent
	agent, err := llmchat.NewMultiToolAgent(cfg)
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

func NewAssistant(clientId, clientSecret, cardTemplateId string, uc redis.UniversalClient, chat *llmchat.Chat) (*Assistant, error) {
	card, err := dingtalk.NewCardSender(clientId, clientSecret, cardTemplateId, uc)
	if err != nil {
		return nil, err
	}

	bot := dingtalk.NewBot(clientId, clientSecret, chat, card)
	return &Assistant{bot: bot}, nil
}
