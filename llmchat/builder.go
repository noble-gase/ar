package llmchat

import (
	"context"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/noble-gase/neon/httpkit"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/agenttool"
	"google.golang.org/adk/v2/tool/mcptoolset"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"
)

// AgentBuilder 是构造 agent 的接口。
type AgentBuilder interface {
	Build(model.LLM) (agent.Agent, error)
}

// staticToolset 把一组固定的工具适配成 tool.Toolset，这样那些作用于 toolset
// 的辅助函数（如 tool.WithConfirmation）就能用在单独构造的工具上，比如
// agent-as-tool。
type staticToolset struct {
	name  string
	tools []tool.Tool
}

func (s *staticToolset) Name() string { return s.name }

func (s *staticToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	return s.tools, nil
}

// resolveModel 返回子 agent 使用的模型：设置了 adapter 就用 adapter 的模型，
// 否则用根模型。
func resolveModel(llm model.LLM, adapter LLMAdapter) (model.LLM, error) {
	if adapter == nil {
		return llm, nil
	}

	m, err := adapter.Model()
	if err != nil {
		return nil, fmt.Errorf("LLM Model: %w", err)
	}
	return m, nil
}

// wrapConfirmation 在 require 为 true 或设置了 provider 时，给 toolset 套上
// 人工确认（HITL）。否则原样返回。
func wrapConfirmation(ts tool.Toolset, require bool, provider tool.ConfirmationProvider) tool.Toolset {
	if require || provider != nil {
		return tool.WithConfirmation(ts, require, provider)
	}
	return ts
}

// MCPSource 配置一个 MCP 服务（Streamable HTTP），以及它自己的人工确认策略。
type MCPSource struct {
	// Endpoint 是 MCP 服务的 Streamable HTTP 端点。
	Endpoint string

	// RequireConfirmation 为 true 时，该端点暴露的每个工具执行前都要人工确认。
	RequireConfirmation bool

	// ConfirmationProvider 动态决定该端点的某次具体工具调用是否需要确认。
	// 设置后优先级高于 RequireConfirmation。
	ConfirmationProvider tool.ConfirmationProvider
}

// buildMCPServer 为单个 MCP 服务（Streamable HTTP）构造带确认能力的工具集。
func buildMCPServer(server MCPSource) (tool.Toolset, error) {
	transport := &mcp.StreamableClientTransport{
		Endpoint:   server.Endpoint,
		HTTPClient: httpkit.NewHttpClient(),
	}
	toolset, err := mcptoolset.New(mcptoolset.Config{
		Transport: transport,
	})
	if err != nil {
		return nil, fmt.Errorf("MCP Toolset: %w", err)
	}
	return wrapConfirmation(toolset, server.RequireConfirmation, server.ConfirmationProvider), nil
}

// SkillSource 配置一个技能目录，以及它自己的人工确认策略。该目录下的每个
// 一级子目录即为一个技能，例如：
//
//	skill-1/
//	    SKILL.md
//	    assets/
//	skill-2/
//	    SKILL.md
//	    references/
//	    scripts/
type SkillSource struct {
	// Path 是技能目录路径。
	Path string

	// RequireConfirmation 为 true 时，从该目录加载的每个技能工具执行前都要人工确认。
	RequireConfirmation bool

	// ConfirmationProvider 动态决定该目录下的某个技能工具是否需要确认。
	// 设置后优先级高于 RequireConfirmation。
	ConfirmationProvider tool.ConfirmationProvider
}

// buildSkill 为单个技能目录构造带确认能力的工具集。
func buildSkill(ctx context.Context, sk SkillSource) (tool.Toolset, error) {
	source := skill.NewFileSystemSource(os.DirFS(sk.Path))
	source, _, err := skill.WithCompletePreloadSource(ctx, source)
	if err != nil {
		return nil, fmt.Errorf("Skill Toolset: %w", err)
	}

	toolset, err := skilltoolset.New(ctx, skilltoolset.Config{Source: source})
	if err != nil {
		return nil, fmt.Errorf("Skill Toolset: %w", err)
	}
	return wrapConfirmation(toolset, sk.RequireConfirmation, sk.ConfirmationProvider), nil
}

// AgentToolSource 把一个 agent 以工具形式暴露（agent-as-tool），并带上它自己的
// 人工确认策略。
//
// 确认粒度是「调用整个子 agent」这一步：子 agent 从父的 runner/session 发起，
// 确认事件能冒泡到顶层。它不会确认子 agent「内部工具」（那些在隔离
// sub-runner 里运行）。若需确认内部某个具体工具（如带 device_id 的操作），
// 请把该工具直接挂到 NormalAgent 的 Tools/MCPServers/Skills，或用 workflow
// SubAgents。
type AgentToolSource struct {
	// Agent 构造以工具形式暴露的子 agent。
	Agent AgentBuilder

	// RequireConfirmation 为 true 时，调用这个子 agent 前要先人工确认。
	RequireConfirmation bool

	// ConfirmationProvider 动态决定这次子 agent 调用是否需要确认。它拿到的是子
	// agent（工具）的名字，设置后优先级高于 RequireConfirmation。
	ConfirmationProvider tool.ConfirmationProvider
}

// buildAgentTool 构造把单个 agent 以工具形式暴露（agent-as-tool）的工具集，并
// 带确认能力。确认拦的是整个子 agent 调用，它跑在父级的 runner/session 里，
// 所以确认事件能冒泡上来；它**不会**去确认子 agent 内部的工具（那些跑在隔离的
// 子 runner 里）。
func buildAgentTool(llm model.LLM, src AgentToolSource) (tool.Toolset, error) {
	subAgent, err := src.Agent.Build(llm)
	if err != nil {
		return nil, fmt.Errorf("Agent Tool: %w", err)
	}

	ts := &staticToolset{
		name:  "agent:" + subAgent.Name(),
		tools: []tool.Tool{agenttool.New(subAgent, nil)},
	}
	return wrapConfirmation(ts, src.RequireConfirmation, src.ConfirmationProvider), nil
}

// buildSubAgents 用解析出的模型构造所有子 agent。
func buildSubAgents(llm model.LLM, builders []AgentBuilder) ([]agent.Agent, error) {
	subAgents := make([]agent.Agent, 0, len(builders))
	for _, v := range builders {
		subAgent, err := v.Build(llm)
		if err != nil {
			return nil, fmt.Errorf("Sub-Agent: %w", err)
		}
		subAgents = append(subAgents, subAgent)
	}
	return subAgents, nil
}
