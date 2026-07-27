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

// AgentBuilder is an interface that builds an agent.
type AgentBuilder interface {
	Build(model.LLM) (agent.Agent, error)
}

// staticToolset adapts a fixed list of tools into a tool.Toolset, so helpers
// that operate on toolsets (e.g. tool.WithConfirmation) can be applied to
// individually constructed tools such as agent-as-tool.
type staticToolset struct {
	name  string
	tools []tool.Tool
}

func (s *staticToolset) Name() string { return s.name }

func (s *staticToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	return s.tools, nil
}

// resolveModel returns the model used for sub-agents: the adapter's model when
// an adapter is set, otherwise the root model.
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

// wrapConfirmation wraps a toolset with Human-in-the-Loop (HITL) confirmation
// when either require is true or a provider is set. Otherwise the toolset is
// returned unchanged.
func wrapConfirmation(ts tool.Toolset, require bool, provider tool.ConfirmationProvider) tool.Toolset {
	if require || provider != nil {
		return tool.WithConfirmation(ts, require, provider)
	}
	return ts
}

// MCPSource configures a single MCP server (Streamable HTTP) together with its
// own Human-in-the-Loop confirmation policy.
type MCPSource struct {
	// Endpoint is the Streamable HTTP endpoint of the MCP server.
	Endpoint string

	// RequireConfirmation, when true, requests HITL confirmation for every tool
	// exposed by this endpoint before execution.
	RequireConfirmation bool

	// ConfirmationProvider dynamically decides whether a specific tool call from
	// this endpoint needs confirmation. It takes precedence over
	// RequireConfirmation when set.
	ConfirmationProvider tool.ConfirmationProvider
}

// buildMCPServer builds a confirmation-aware toolset for a single MCP server
// (Streamable HTTP).
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

// SkillSource configures a single skills directory together with its own
// Human-in-the-Loop confirmation policy. The directory is expected to contain
// skills as immediate subdirectories, for example:
//
//	  skill-1/
//		   SKILL.md
//		   assets/
//	  skill-2/
//		   SKILL.md
//		   references/
//		   scripts/
type SkillSource struct {
	// Path is the skills directory path.
	Path string

	// RequireConfirmation, when true, requests HITL confirmation for every skill
	// tool loaded from this directory before execution.
	RequireConfirmation bool

	// ConfirmationProvider dynamically decides whether a specific skill tool from
	// this directory needs confirmation. It takes precedence over
	// RequireConfirmation when set.
	ConfirmationProvider tool.ConfirmationProvider
}

// buildSkill builds a confirmation-aware toolset for a single skills directory.
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

// AgentToolSource exposes a single agent as a tool (agent-as-tool), together
// with its own Human-in-the-Loop confirmation policy.
//
// 确认粒度是「调用整个子 agent」这一步：子 agent 从父的 runner/session 发起，
// 确认事件能冒泡到顶层。它不会确认子 agent「内部工具」（那些在隔离
// sub-runner 里运行）。若需确认内部某个具体工具（如带 device_id 的操作），
// 请把该工具直接挂到 NormalAgent 的 Tools/MCPServers/Skills，或用 workflow
// SubAgents。
type AgentToolSource struct {
	// Agent builds the sub-agent exposed as a tool.
	Agent AgentBuilder

	// RequireConfirmation, when true, requests HITL confirmation before calling
	// this sub-agent.
	RequireConfirmation bool

	// ConfirmationProvider dynamically decides whether this sub-agent call needs
	// confirmation. It receives the sub-agent (tool) name and takes precedence
	// over RequireConfirmation when set.
	ConfirmationProvider tool.ConfirmationProvider
}

// buildAgentTool builds a confirmation-aware toolset exposing a single agent as
// a tool (agent-as-tool). Confirmation gates the whole sub-agent call, which
// runs in the parent runner/session so the confirmation event bubbles up; it
// does NOT confirm the sub-agent's internal tools (those run in an isolated
// sub-runner).
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

// buildSubAgents builds all sub-agent builders with the resolved model.
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
