package argon

import (
	"github.com/noble-gase/argon/channel/dingtalk"
	"github.com/noble-gase/argon/llmchat"
	"github.com/noble-gase/argon/session"
	"github.com/redis/go-redis/v9"
	"google.golang.org/adk/v2/agent"
	"gorm.io/gorm"
)

// NewLLMAgent 返回一个 LLM agent。
func NewLLMAgent(builder llmchat.AgentBuilder) (agent.Agent, error) {
	return builder.Build(nil)
}

// NewLLMChat 返回一个 LLM chat。opts 透传给 session.New，多实例部署应通过
// session.WithLocation 显式统一自动会话的轮换时区。
func NewLLMChat(name string, db gorm.Dialector, builder llmchat.AgentBuilder, opts ...session.Option) (*llmchat.Chat, error) {
	// Agent
	agent, err := builder.Build(nil)
	if err != nil {
		return nil, err
	}

	// Session
	session, err := session.New(name, db, opts...)
	if err != nil {
		return nil, err
	}

	// Chat
	return llmchat.NewChat(agent, session)
}

type DingTalkAssistant struct {
	bot *dingtalk.Bot
}

func (dta *DingTalkAssistant) Start() error {
	return dta.bot.Start()
}

func (dta *DingTalkAssistant) Stop() {
	dta.bot.Stop()
}

// NewDingTalkAssistant 返回一个钉钉助手。
func NewDingTalkAssistant(cfg *dingtalk.Config, uc redis.UniversalClient, chat *llmchat.Chat) (*DingTalkAssistant, error) {
	card, err := dingtalk.NewCardSender(cfg, uc)
	if err != nil {
		return nil, err
	}

	bot := dingtalk.NewBot(cfg, chat, card)
	return &DingTalkAssistant{bot: bot}, nil
}
