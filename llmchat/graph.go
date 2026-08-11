package llmchat

import (
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/workflow"
)

// Graph 是传给 GraphAgent.Edges 的节点工厂。Agent 节点必须经由它构建，因为
// AgentBuilder 需要的模型要到 GraphAgent.Build 时才能确定；它同时收集这些 Agent
// 填入 workflowagent 的 SubAgents，否则 runner 解析不了它们发出的事件作者。
type Graph struct {
	llm    model.LLM
	agents []agent.Agent
	seq    int
	err    error
}

// Agent 返回包装一个 Agent 的节点，节点名即 Agent 名。被包装的 Agent 应通过
// Event.Output 输出最终结果，才能传递给后继节点。错误会延迟到 GraphAgent 构建
// 时统一报出。
func (g *Graph) Agent(builder AgentBuilder, cfg workflow.NodeConfig) workflow.Node {
	subAgent, err := builder.Build(g.llm)
	if err != nil {
		return g.fail(err)
	}

	node, err := workflow.NewAgentNode(subAgent, cfg)
	if err != nil {
		return g.fail(err)
	}

	g.agents = append(g.agents, subAgent)
	return node
}

// fail 记录首个错误并返回一个占位节点，让调用方能继续连边而不会空指针。
func (g *Graph) fail(err error) workflow.Node {
	if g.err == nil {
		g.err = fmt.Errorf("Graph Node: %w", err)
	}
	g.seq++

	name := fmt.Sprintf("__invalid_%d__", g.seq)
	return workflow.NewFunctionNode(name, func(agent.Context, any) (any, error) {
		return nil, g.err
	}, workflow.NodeConfig{})
}

// GraphAgent 构建图工作流 Agent：节点连成有向图，支持条件分支、扇出与扇入
// （workflow.JoinNode）。
//
// 与 SequentialAgent/LoopAgent/ParallelAgent 不同，图工作流的运行状态会持久化到
// session：节点可以通过 RequestInput 暂停，后续轮次凭 InterruptID 精确恢复并把
// 回复交给后继节点，因此人工确认（HITL）不会导致前置节点被重复执行。
type GraphAgent struct {
	Name        string
	Description string

	// LLMAdapter 指定 Agent 节点使用的模型，未设置时使用根 Agent 的模型。
	LLMAdapter LLMAdapter

	// Edges 负责连边：Agent 节点用 Graph.Agent 构建，其余节点用
	// workflow.NewFunctionNode / workflow.NewJoinNode 构建，再用 workflow.Chain /
	// workflow.Concat / workflow.NewEdgeBuilder 从 workflow.Start 开始组装。
	Edges func(g *Graph) []workflow.Edge

	AgentHooks AgentCallback
}

func (ga *GraphAgent) Build(llm model.LLM) (agent.Agent, error) {
	llm, err := resolveModel(llm, ga.LLMAdapter)
	if err != nil {
		return nil, err
	}

	if ga.Edges == nil {
		return nil, fmt.Errorf("Graph Agent: Edges is required")
	}

	g := &Graph{llm: llm}
	edges := ga.Edges(g)
	if g.err != nil {
		return nil, g.err
	}

	return workflowagent.New(workflowagent.Config{
		Name:                 ga.Name,
		Description:          ga.Description,
		Edges:                edges,
		SubAgents:            g.agents,
		BeforeAgentCallbacks: ga.AgentHooks.Before,
		AfterAgentCallbacks:  ga.AgentHooks.After,
	})
}
