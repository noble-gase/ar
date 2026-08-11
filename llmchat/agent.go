package llmchat

import (
	"context"
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
)

type AgentCallback struct {
	Before []agent.BeforeAgentCallback
	After  []agent.AfterAgentCallback
}

type ToolCallback struct {
	Before []llmagent.BeforeToolCallback
	After  []llmagent.AfterToolCallback
	Error  []llmagent.OnToolErrorCallback
}

type ModelCallback struct {
	Before []llmagent.BeforeModelCallback
	After  []llmagent.AfterModelCallback
	Error  []llmagent.OnModelErrorCallback
}

// NormalAgent 是通用的 agent 构造器。一个 NormalAgent 可以混用所有工具类型：
//   - Tools:      函数工具（FuncTool）
//   - Skills:     从目录加载的技能工具集
//   - MCPServers: MCP 服务（Streamable HTTP）
//   - AgentTools: 以工具形式暴露的其它 agent（agent-as-tool）
//
// 每种来源都各自带有人工确认（HITL）策略。这些字段都是可选的，某类工具不需要
// 就把切片留空。它可以作为根 agent、工作流的 SubAgent，或者（通过 AgentTools）
// 作为以工具形式暴露的子 agent。
type NormalAgent struct {
	Name        string
	Description string
	Instruction string

	// LLMAdapter 指定该 agent 使用的模型，未设置时沿用根 agent 的模型。
	LLMAdapter LLMAdapter

	// Tools 是函数工具（FuncTool）列表，每个工具各自带确认策略。
	Tools []ToolBuilder

	// Skills 是技能目录列表，每个各自带确认策略。
	Skills []SkillSource

	// MCPServers 是 MCP 服务（Streamable HTTP）列表，每个各自带确认策略。
	MCPServers []MCPSource

	// AgentTools 把其它 agent 以工具形式暴露（agent-as-tool），每个各自带确认
	// 策略。这让一个 NormalAgent 能同时混用函数工具、技能、MCP 服务和子 agent。
	AgentTools []AgentToolSource

	// SubAgents 通过 LLM 驱动的转交（transfer_to_agent）暴露子 agent。
	// 与 AgentTools（agent-as-tool）不同，被转交的子 agent 跑在与父级**相同**的
	// runner/session 里，因此子 agent 自己的工具发起的人工确认能冒泡上来并被恢复。
	// 子 agent 的工具只有在转交之后才会声明给模型，所以在用到之前，这些工具声明
	// 不会占用父级的上下文。
	SubAgents []AgentBuilder

	AgentHooks AgentCallback
	ToolHooks  ToolCallback
	ModelHooks ModelCallback

	// OutputKey 仅用于工作流编排。
	OutputKey string
}

func (na *NormalAgent) Build(llm model.LLM) (agent.Agent, error) {
	ctx := context.Background()

	cfg := llmagent.Config{
		Name:                  na.Name,
		Description:           na.Description,
		Instruction:           na.Instruction,
		Tools:                 make([]tool.Tool, 0, len(na.Tools)),
		Toolsets:              make([]tool.Toolset, 0, len(na.MCPServers)+len(na.Skills)+len(na.AgentTools)),
		BeforeAgentCallbacks:  na.AgentHooks.Before,
		AfterAgentCallbacks:   na.AgentHooks.After,
		BeforeToolCallbacks:   na.ToolHooks.Before,
		AfterToolCallbacks:    na.ToolHooks.After,
		OnToolErrorCallbacks:  na.ToolHooks.Error,
		BeforeModelCallbacks:  na.ModelHooks.Before,
		AfterModelCallbacks:   na.ModelHooks.After,
		OnModelErrorCallbacks: na.ModelHooks.Error,
		OutputKey:             na.OutputKey,
	}

	// 工具
	for _, builder := range na.Tools {
		tool, err := builder.Build()
		if err != nil {
			return nil, fmt.Errorf("Tools: %w", err)
		}
		cfg.Tools = append(cfg.Tools, tool)
	}

	// 技能工具集
	for _, sk := range na.Skills {
		toolset, err := buildSkill(ctx, sk)
		if err != nil {
			return nil, err
		}
		cfg.Toolsets = append(cfg.Toolsets, toolset)
	}

	// MCP 工具集（Streamable HTTP）
	for _, server := range na.MCPServers {
		toolset, err := buildMCPServer(server)
		if err != nil {
			return nil, err
		}
		cfg.Toolsets = append(cfg.Toolsets, toolset)
	}

	// LLM 模型
	llm, err := resolveModel(llm, na.LLMAdapter)
	if err != nil {
		return nil, err
	}
	cfg.Model = llm

	// Agent Tools（子 agent 以 agent-as-tool 暴露；需在 cfg.Model 确定后构建）
	for _, src := range na.AgentTools {
		toolset, err := buildAgentTool(llm, src)
		if err != nil {
			return nil, err
		}
		cfg.Toolsets = append(cfg.Toolsets, toolset)
	}

	// Sub Agents（transfer 型委派：与根共享 runner/session，子 agent 内工具的
	// 人工确认可正常冒泡与恢复；工具仅在 transfer 后声明，同样按需省 token）
	if len(na.SubAgents) > 0 {
		subAgents, err := buildSubAgents(llm, na.SubAgents)
		if err != nil {
			return nil, err
		}
		cfg.SubAgents = subAgents
	}

	return llmagent.New(cfg)
}
