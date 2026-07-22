package llmchat

import (
	"context"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/noble-gase/neon/httpkit"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/agenttool"
	"google.golang.org/adk/v2/tool/mcptoolset"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"
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

// AgentBuilder is an interface that builds an agent.
type AgentBuilder interface {
	Build(model.LLM) (agent.Agent, error)
}

// NormalAgent builds an agent with MCP toolsets and function tools.
type NormalAgent struct {
	Name        string
	Description string
	Instruction string

	// LLMAdapter specifies the model for agent, if not set, the root agent model will be used.
	LLMAdapter LLMAdapter

	// Endpoints is the list endpoints of MCP servers based on Streamable HTTP.
	Endpoints []string

	// Skills is the list of skills directory paths, which expected layout example:
	//
	//	  skill-1/
	//		   SKILL.md
	//		   assets/
	//	  skill-2/
	//		   SKILL.md
	//		   references/
	//		   scripts/
	Skills []string

	Tools []ToolBuilder

	AgentHooks AgentCallback
	ToolHooks  ToolCallback
	ModelHooks ModelCallback

	// OutputKey only used for workflow coordination.
	OutputKey string
}

func (n *NormalAgent) Build(rootModel model.LLM) (agent.Agent, error) {
	ctx := context.Background()

	cfg := llmagent.Config{
		Name:                  n.Name,
		Description:           n.Description,
		Instruction:           n.Instruction,
		Tools:                 make([]tool.Tool, 0, len(n.Tools)),
		Toolsets:              make([]tool.Toolset, 0, len(n.Endpoints)+len(n.Skills)),
		BeforeAgentCallbacks:  n.AgentHooks.Before,
		AfterAgentCallbacks:   n.AgentHooks.After,
		BeforeToolCallbacks:   n.ToolHooks.Before,
		AfterToolCallbacks:    n.ToolHooks.After,
		OnToolErrorCallbacks:  n.ToolHooks.Error,
		BeforeModelCallbacks:  n.ModelHooks.Before,
		AfterModelCallbacks:   n.ModelHooks.After,
		OnModelErrorCallbacks: n.ModelHooks.Error,
		OutputKey:             n.OutputKey,
	}

	// MCP Toolset (Streamable HTTP)
	for _, endpoint := range n.Endpoints {
		transport := &mcp.StreamableClientTransport{
			Endpoint:   endpoint,
			HTTPClient: httpkit.NewHttpClient(),
		}
		toolset, err := mcptoolset.New(mcptoolset.Config{
			Transport: transport,
		})
		if err != nil {
			return nil, fmt.Errorf("MCP Toolset: %w", err)
		}
		cfg.Toolsets = append(cfg.Toolsets, toolset)
	}

	// Skill Toolset
	for _, dir := range n.Skills {
		source := skill.NewFileSystemSource(os.DirFS(dir))
		source, _, err := skill.WithCompletePreloadSource(ctx, source)
		if err != nil {
			return nil, fmt.Errorf("Skill Toolset: %w", err)
		}

		toolset, err := skilltoolset.New(ctx, skilltoolset.Config{Source: source})
		if err != nil {
			return nil, fmt.Errorf("Skill Toolset: %w", err)
		}
		cfg.Toolsets = append(cfg.Toolsets, toolset)
	}

	// Tools
	for _, builder := range n.Tools {
		tool, err := builder.Build()
		if err != nil {
			return nil, fmt.Errorf("Tools: %w", err)
		}
		cfg.Tools = append(cfg.Tools, tool)
	}

	// LLM Model
	if n.LLMAdapter != nil {
		llmModel, err := n.LLMAdapter.Model()
		if err != nil {
			return nil, fmt.Errorf("LLM Model: %w", err)
		}
		cfg.Model = llmModel
	} else {
		cfg.Model = rootModel
	}

	return llmagent.New(cfg)
}

// FuncAgent builds an agent with function tools.
type FuncAgent struct {
	Name        string
	Description string
	Instruction string

	// LLMAdapter specifies the model for agent, if not set, the root agent model will be used.
	LLMAdapter LLMAdapter

	Tools []ToolBuilder

	AgentHooks AgentCallback
	ToolHooks  ToolCallback
	ModelHooks ModelCallback

	// OutputKey only used for workflow coordination.
	OutputKey string
}

func (f *FuncAgent) Build(rootModel model.LLM) (agent.Agent, error) {
	tools := make([]tool.Tool, 0, len(f.Tools))
	for _, v := range f.Tools {
		t, err := v.Build()
		if err != nil {
			return nil, fmt.Errorf("Tools: %w", err)
		}
		tools = append(tools, t)
	}

	cfg := llmagent.Config{
		Name:                  f.Name,
		Description:           f.Description,
		Instruction:           f.Instruction,
		Tools:                 tools,
		BeforeAgentCallbacks:  f.AgentHooks.Before,
		AfterAgentCallbacks:   f.AgentHooks.After,
		BeforeToolCallbacks:   f.ToolHooks.Before,
		AfterToolCallbacks:    f.ToolHooks.After,
		OnToolErrorCallbacks:  f.ToolHooks.Error,
		BeforeModelCallbacks:  f.ModelHooks.Before,
		AfterModelCallbacks:   f.ModelHooks.After,
		OnModelErrorCallbacks: f.ModelHooks.Error,
		OutputKey:             f.OutputKey,
	}

	// LLM Model
	if f.LLMAdapter != nil {
		llmModel, err := f.LLMAdapter.Model()
		if err != nil {
			return nil, fmt.Errorf("LLM Model: %w", err)
		}
		cfg.Model = llmModel
	} else {
		cfg.Model = rootModel
	}

	return llmagent.New(cfg)
}

// SkillAgent builds an agent with skill toolsets.
type SkillAgent struct {
	Name string

	Description string
	Instruction string

	// LLMAdapter specifies the model for agent, if not set, the root agent model will be used.
	LLMAdapter LLMAdapter

	// Skills is the list of skills directory paths, which expected layout example:
	//
	//	  skill-1/
	//		   SKILL.md
	//		   assets/
	//	  skill-2/
	//		   SKILL.md
	//		   references/
	//		   scripts/
	Skills []string

	AgentHooks AgentCallback
	ToolHooks  ToolCallback
	ModelHooks ModelCallback

	// OutputKey only used for workflow coordination.
	OutputKey string
}

func (s *SkillAgent) Build(rootModel model.LLM) (agent.Agent, error) {
	ctx := context.Background()

	cfg := llmagent.Config{
		Name:                  s.Name,
		Description:           s.Description,
		Instruction:           s.Instruction,
		Toolsets:              make([]tool.Toolset, 0, len(s.Skills)),
		BeforeAgentCallbacks:  s.AgentHooks.Before,
		AfterAgentCallbacks:   s.AgentHooks.After,
		BeforeToolCallbacks:   s.ToolHooks.Before,
		AfterToolCallbacks:    s.ToolHooks.After,
		OnToolErrorCallbacks:  s.ToolHooks.Error,
		BeforeModelCallbacks:  s.ModelHooks.Before,
		AfterModelCallbacks:   s.ModelHooks.After,
		OnModelErrorCallbacks: s.ModelHooks.Error,
		OutputKey:             s.OutputKey,
	}

	// Skill Toolset
	for _, dir := range s.Skills {
		source := skill.NewFileSystemSource(os.DirFS(dir))
		source, _, err := skill.WithCompletePreloadSource(ctx, source)
		if err != nil {
			return nil, fmt.Errorf("Toolset: %w", err)
		}

		toolset, err := skilltoolset.New(ctx, skilltoolset.Config{Source: source})
		if err != nil {
			return nil, fmt.Errorf("Toolset: %w", err)
		}
		cfg.Toolsets = append(cfg.Toolsets, toolset)
	}

	// LLM Model
	if s.LLMAdapter != nil {
		llmModel, err := s.LLMAdapter.Model()
		if err != nil {
			return nil, fmt.Errorf("LLM Model: %w", err)
		}
		cfg.Model = llmModel
	} else {
		cfg.Model = rootModel
	}

	return llmagent.New(cfg)
}

// MCPAgent builds an agent with MCP toolsets.
type MCPAgent struct {
	Name string

	Description string
	Instruction string

	// LLMAdapter specifies the model for agent, if not set, the root agent model will be used.
	LLMAdapter LLMAdapter

	// Endpoints is the list endpoints of MCP servers based on Streamable HTTP.
	Endpoints []string

	AgentHooks AgentCallback
	ToolHooks  ToolCallback
	ModelHooks ModelCallback

	// OutputKey only used for workflow coordination.
	OutputKey string
}

func (m *MCPAgent) Build(rootModel model.LLM) (agent.Agent, error) {
	cfg := llmagent.Config{
		Name:                  m.Name,
		Description:           m.Description,
		Instruction:           m.Instruction,
		Toolsets:              make([]tool.Toolset, 0, len(m.Endpoints)),
		BeforeAgentCallbacks:  m.AgentHooks.Before,
		AfterAgentCallbacks:   m.AgentHooks.After,
		BeforeToolCallbacks:   m.ToolHooks.Before,
		AfterToolCallbacks:    m.ToolHooks.After,
		OnToolErrorCallbacks:  m.ToolHooks.Error,
		BeforeModelCallbacks:  m.ModelHooks.Before,
		AfterModelCallbacks:   m.ModelHooks.After,
		OnModelErrorCallbacks: m.ModelHooks.Error,
		OutputKey:             m.OutputKey,
	}

	// MCP Toolset (Streamable HTTP)
	for _, endpoint := range m.Endpoints {
		transport := &mcp.StreamableClientTransport{
			Endpoint:   endpoint,
			HTTPClient: httpkit.NewHttpClient(),
		}
		toolset, err := mcptoolset.New(mcptoolset.Config{
			Transport: transport,
		})
		if err != nil {
			return nil, fmt.Errorf("Toolset: %w", err)
		}
		cfg.Toolsets = append(cfg.Toolsets, toolset)
	}

	// LLM Model
	if m.LLMAdapter != nil {
		llmModel, err := m.LLMAdapter.Model()
		if err != nil {
			return nil, fmt.Errorf("LLM Model: %w", err)
		}
		cfg.Model = llmModel
	} else {
		cfg.Model = rootModel
	}

	return llmagent.New(cfg)
}

// AgentTool builds an agent with other agents as tools.
type AgentTool struct {
	Name string

	Description string
	Instruction string

	// LLMAdapter specifies the model for agent, if not set, the root agent model will be used.
	LLMAdapter LLMAdapter

	Tools []AgentBuilder

	AgentHooks AgentCallback
	ToolHooks  ToolCallback
	ModelHooks ModelCallback

	// OutputKey only used for workflow coordination.
	OutputKey string
}

func (a *AgentTool) Build(rootModel model.LLM) (agent.Agent, error) {
	cfg := llmagent.Config{
		Name:                  a.Name,
		Description:           a.Description,
		Instruction:           a.Instruction,
		Tools:                 make([]tool.Tool, 0, len(a.Tools)),
		BeforeAgentCallbacks:  a.AgentHooks.Before,
		AfterAgentCallbacks:   a.AgentHooks.After,
		BeforeToolCallbacks:   a.ToolHooks.Before,
		AfterToolCallbacks:    a.ToolHooks.After,
		OnToolErrorCallbacks:  a.ToolHooks.Error,
		BeforeModelCallbacks:  a.ModelHooks.Before,
		AfterModelCallbacks:   a.ModelHooks.After,
		OnModelErrorCallbacks: a.ModelHooks.Error,
		OutputKey:             a.OutputKey,
	}

	// LLM Model
	if a.LLMAdapter != nil {
		llmModel, err := a.LLMAdapter.Model()
		if err != nil {
			return nil, fmt.Errorf("LLM Model: %w", err)
		}
		cfg.Model = llmModel
	} else {
		cfg.Model = rootModel
	}

	// Tools
	for _, v := range a.Tools {
		agent, err := v.Build(cfg.Model)
		if err != nil {
			return nil, fmt.Errorf("Agent(Tool): %w", err)
		}
		cfg.Tools = append(cfg.Tools, agenttool.New(agent, nil))
	}

	return llmagent.New(cfg)
}
