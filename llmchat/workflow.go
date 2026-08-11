package llmchat

import (
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagents/loopagent"
	"google.golang.org/adk/v2/agent/workflowagents/parallelagent"
	"google.golang.org/adk/v2/agent/workflowagents/sequentialagent"
	"google.golang.org/adk/v2/model"
)

// SequentialAgent 构造一个按顺序运行其子 agent 的 agent。
//
// 人工确认（HITL）限制：workflow 子 agent 与根共享 runner/session，子 agent 里
// 工具触发的确认事件能冒泡出卡片；但 workflow 的 Run 是「每次从头迭代子 agent」，
// 没有暂停点续跑机制。因此确认后的恢复会从第一个子 agent 重新跑，多步顺序中若
// 中间步骤需要确认，前面步骤会被重复执行，不可靠。需要稳定 HITL 的工具请放到
// 单个 NormalAgent 上。
type SequentialAgent struct {
	Name        string
	Description string

	// LLMAdapter 指定子 agent 使用的模型，未设置时沿用根 agent 的模型。
	LLMAdapter LLMAdapter

	SubAgents []AgentBuilder

	AgentHooks AgentCallback
}

func (sa *SequentialAgent) Build(llm model.LLM) (agent.Agent, error) {
	llm, err := resolveModel(llm, sa.LLMAdapter)
	if err != nil {
		return nil, err
	}

	subAgents, err := buildSubAgents(llm, sa.SubAgents)
	if err != nil {
		return nil, err
	}

	return sequentialagent.New(sequentialagent.Config{
		AgentConfig: agent.Config{
			Name:                 sa.Name,
			Description:          sa.Description,
			SubAgents:            subAgents,
			BeforeAgentCallbacks: sa.AgentHooks.Before,
			AfterAgentCallbacks:  sa.AgentHooks.After,
		},
	})
}

// LoopAgent 构造一个反复运行其子 agent 的 agent，直到达到指定轮数或满足终止条件。
//
// 人工确认（HITL）限制：同 SequentialAgent。确认事件能冒泡出卡片，但恢复会从头
// 重跑子 agent，循环 + 确认的暂停/恢复不可靠。需要稳定 HITL 的工具请放到单个
// NormalAgent 上。
type LoopAgent struct {
	Name        string
	Description string

	// LLMAdapter 指定子 agent 使用的模型，未设置时沿用根 agent 的模型。
	LLMAdapter LLMAdapter

	SubAgents []AgentBuilder

	AgentHooks AgentCallback

	MaxIterations uint
}

func (la *LoopAgent) Build(llm model.LLM) (agent.Agent, error) {
	llm, err := resolveModel(llm, la.LLMAdapter)
	if err != nil {
		return nil, err
	}

	subAgents, err := buildSubAgents(llm, la.SubAgents)
	if err != nil {
		return nil, err
	}

	return loopagent.New(loopagent.Config{
		AgentConfig: agent.Config{
			Name:                 la.Name,
			Description:          la.Description,
			SubAgents:            subAgents,
			BeforeAgentCallbacks: la.AgentHooks.Before,
			AfterAgentCallbacks:  la.AgentHooks.After,
		},
		MaxIterations: la.MaxIterations,
	})
}

// ParallelAgent 构造一个并行运行其子 agent 的 agent。
//
// 人工确认（HITL）限制：并发子 agent 可能同时产生多个确认，ADK 未定义其恢复
// 路由，基本无法可靠使用。需要人工确认的工具不要放在 ParallelAgent 下，改用
// 单个 NormalAgent。
type ParallelAgent struct {
	Name        string
	Description string

	// LLMAdapter 指定子 agent 使用的模型，未设置时沿用根 agent 的模型。
	LLMAdapter LLMAdapter

	SubAgents []AgentBuilder

	AgentHooks AgentCallback
}

func (pa *ParallelAgent) Build(llm model.LLM) (agent.Agent, error) {
	llm, err := resolveModel(llm, pa.LLMAdapter)
	if err != nil {
		return nil, err
	}

	subAgents, err := buildSubAgents(llm, pa.SubAgents)
	if err != nil {
		return nil, err
	}

	return parallelagent.New(parallelagent.Config{
		AgentConfig: agent.Config{
			Name:                 pa.Name,
			Description:          pa.Description,
			SubAgents:            subAgents,
			BeforeAgentCallbacks: pa.AgentHooks.Before,
			AfterAgentCallbacks:  pa.AgentHooks.After,
		},
	})
}
