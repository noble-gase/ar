package llmchat

import (
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/workflowagents/loopagent"
	"google.golang.org/adk/agent/workflowagents/parallelagent"
	"google.golang.org/adk/agent/workflowagents/sequentialagent"
	"google.golang.org/adk/model"
)

// SequentialAgent builds an agent that runs its sub-agents in a sequence.
type SequentialAgent struct {
	Name        string
	Description string

	// LLMAdapter specifies the model for sub-agents, if not set, the root agent model will be used.
	LLMAdapter LLMAdapter

	SubAgents []AgentBuilder

	AgentHooks AgentCallback
}

func (s *SequentialAgent) Build(rootModel model.LLM) (agent.Agent, error) {
	cfg := agent.Config{
		Name:                 s.Name,
		Description:          s.Description,
		SubAgents:            make([]agent.Agent, 0, len(s.SubAgents)),
		BeforeAgentCallbacks: s.AgentHooks.Before,
		AfterAgentCallbacks:  s.AgentHooks.After,
	}

	if s.LLMAdapter != nil {
		llmModel, err := s.LLMAdapter.Model()
		if err != nil {
			return nil, err
		}

		for _, v := range s.SubAgents {
			subAgent, _err := v.Build(llmModel)
			if _err != nil {
				return nil, _err
			}
			cfg.SubAgents = append(cfg.SubAgents, subAgent)
		}
	} else {
		for _, v := range s.SubAgents {
			subAgent, _err := v.Build(rootModel)
			if _err != nil {
				return nil, _err
			}
			cfg.SubAgents = append(cfg.SubAgents, subAgent)
		}
	}

	return sequentialagent.New(sequentialagent.Config{
		AgentConfig: cfg,
	})
}

// LoopAgent builds an agent that repeatedly runs its sub-agents for a specified number of iterations or until termination condition is met.
type LoopAgent struct {
	Name        string
	Description string

	// LLMAdapter specifies the model for sub-agents, if not set, the root agent model will be used.
	LLMAdapter LLMAdapter

	SubAgents []AgentBuilder

	AgentHooks AgentCallback

	MaxIterations uint
}

func (l *LoopAgent) Build(rootModel model.LLM) (agent.Agent, error) {
	cfg := agent.Config{
		Name:                 l.Name,
		Description:          l.Description,
		SubAgents:            make([]agent.Agent, 0, len(l.SubAgents)),
		BeforeAgentCallbacks: l.AgentHooks.Before,
		AfterAgentCallbacks:  l.AgentHooks.After,
	}

	if l.LLMAdapter != nil {
		llmModel, err := l.LLMAdapter.Model()
		if err != nil {
			return nil, err
		}

		for _, v := range l.SubAgents {
			subAgent, _err := v.Build(llmModel)
			if _err != nil {
				return nil, _err
			}
			cfg.SubAgents = append(cfg.SubAgents, subAgent)
		}
	} else {
		for _, v := range l.SubAgents {
			subAgent, _err := v.Build(rootModel)
			if _err != nil {
				return nil, _err
			}
			cfg.SubAgents = append(cfg.SubAgents, subAgent)
		}
	}

	return loopagent.New(loopagent.Config{
		AgentConfig:   cfg,
		MaxIterations: l.MaxIterations,
	})
}

// ParallelAgent builds an agent that runs its sub-agents in parallel.
type ParallelAgent struct {
	Name        string
	Description string

	// LLMAdapter specifies the model for sub-agents, if not set, the root agent model will be used.
	LLMAdapter LLMAdapter

	SubAgents []AgentBuilder

	AgentHooks AgentCallback
}

func (p *ParallelAgent) Build(rootModel model.LLM) (agent.Agent, error) {
	cfg := agent.Config{
		Name:                 p.Name,
		Description:          p.Description,
		SubAgents:            make([]agent.Agent, 0, len(p.SubAgents)),
		BeforeAgentCallbacks: p.AgentHooks.Before,
		AfterAgentCallbacks:  p.AgentHooks.After,
	}

	if p.LLMAdapter != nil {
		llmModel, err := p.LLMAdapter.Model()
		if err != nil {
			return nil, err
		}

		for _, v := range p.SubAgents {
			subAgent, _err := v.Build(llmModel)
			if _err != nil {
				return nil, _err
			}
			cfg.SubAgents = append(cfg.SubAgents, subAgent)
		}
	} else {
		for _, v := range p.SubAgents {
			subAgent, _err := v.Build(rootModel)
			if _err != nil {
				return nil, _err
			}
			cfg.SubAgents = append(cfg.SubAgents, subAgent)
		}
	}

	return parallelagent.New(parallelagent.Config{
		AgentConfig: cfg,
	})
}
