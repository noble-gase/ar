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

// NormalAgent is the general-purpose agent builder. A single NormalAgent can
// mix all tool kinds:
//   - Tools:      function tools (FuncTool)
//   - Skills:     skill toolsets loaded from directories
//   - MCPServers: MCP servers (Streamable HTTP)
//   - AgentTools: other agents exposed as tools (agent-as-tool)
//
// Each source carries its own Human-in-the-Loop confirmation policy. Fields are
// optional; leave a slice empty to omit that tool kind. It can serve as the root
// agent, a workflow SubAgent, or (via AgentTools) a sub-agent exposed as a tool.
type NormalAgent struct {
	Name        string
	Description string
	Instruction string

	// LLMAdapter specifies the model for agent, if not set, the root agent model will be used.
	LLMAdapter LLMAdapter

	// Tools is the list of function tools (FuncTool), each with its own
	// confirmation policy.
	Tools []ToolBuilder

	// Skills is the list of skills directories, each with its own confirmation
	// policy.
	Skills []SkillSource

	// MCPServers is the list of MCP servers (Streamable HTTP), each with its own
	// confirmation policy.
	MCPServers []MCPSource

	// AgentTools exposes other agents as tools (agent-as-tool), each with its own
	// confirmation policy. Lets a single NormalAgent mix function tools, skills,
	// MCP servers and sub-agents.
	AgentTools []AgentToolSource

	// SubAgents exposes child agents via LLM-driven delegation (transfer_to_agent).
	// Unlike AgentTools (agent-as-tool), a transferred sub-agent runs in the SAME
	// runner/session as the parent, so Human-in-the-Loop confirmation raised by the
	// sub-agent's own tools bubbles up and can be resumed. A sub-agent's tools are
	// only declared to the model after transfer, so it also keeps tool declarations
	// out of the parent context until needed.
	SubAgents []AgentBuilder

	AgentHooks AgentCallback
	ToolHooks  ToolCallback
	ModelHooks ModelCallback

	// OutputKey only used for workflow coordination.
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

	// Tools
	for _, builder := range na.Tools {
		tool, err := builder.Build()
		if err != nil {
			return nil, fmt.Errorf("Tools: %w", err)
		}
		cfg.Tools = append(cfg.Tools, tool)
	}

	// Skill Toolset
	for _, sk := range na.Skills {
		toolset, err := buildSkill(ctx, sk)
		if err != nil {
			return nil, err
		}
		cfg.Toolsets = append(cfg.Toolsets, toolset)
	}

	// MCP Toolset (Streamable HTTP)
	for _, server := range na.MCPServers {
		toolset, err := buildMCPServer(server)
		if err != nil {
			return nil, err
		}
		cfg.Toolsets = append(cfg.Toolsets, toolset)
	}

	// LLM Model
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
