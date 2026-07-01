package argon

import (
	"github.com/noble-gase/argon/channel/dingtalk"
	"github.com/noble-gase/argon/llmchat"
	"github.com/noble-gase/argon/session"
	"github.com/redis/go-redis/v9"
	"google.golang.org/adk/v2/agent"
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

type DingTalkAssistant struct {
	bot *dingtalk.Bot
}

func (dta *DingTalkAssistant) Start() {
	dta.bot.Start()
}

func (dta *DingTalkAssistant) Stop() {
	dta.bot.Stop()
}

// NewDingTalkAssistant returns a DingTalk assistant.
func NewDingTalkAssistant(cfg *dingtalk.Config, uc redis.UniversalClient, chat *llmchat.Chat) (*DingTalkAssistant, error) {
	card, err := dingtalk.NewCardSender(cfg, uc)
	if err != nil {
		return nil, err
	}

	bot := dingtalk.NewBot(cfg, chat, card)
	return &DingTalkAssistant{bot: bot}, nil
}
