package llmchat

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/noble-gase/ne/helper"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/mcptoolset"
)

type ToolCallback struct {
	Before []llmagent.BeforeToolCallback
	After  []llmagent.AfterToolCallback
}

type ModelCallback struct {
	Before []llmagent.BeforeModelCallback
	After  []llmagent.AfterModelCallback
}

type NormalConfig struct {
	Name        string
	Description string
	Instruction string
	LLMAdapter  LLMAdapter
	MCPServers  []string // Streamable HTTP
	FuncTools   []ToolBuilder
	ToolHooks   ToolCallback
	ModelHooks  ModelCallback
}

func NewNormalAgent(cfg *NormalConfig) (agent.Agent, error) {
	// MCP Toolset (Streamable HTTP)
	toolsets := make([]tool.Toolset, 0, len(cfg.MCPServers))
	for _, endpoint := range cfg.MCPServers {
		transport := &mcp.StreamableClientTransport{
			Endpoint:   endpoint,
			HTTPClient: helper.NewHttpClient(),
		}
		toolset, err := mcptoolset.New(mcptoolset.Config{
			Transport: transport,
		})
		if err != nil {
			return nil, err
		}
		toolsets = append(toolsets, toolset)
	}

	// Func Tools
	tools := make([]tool.Tool, 0, len(cfg.FuncTools))
	for _, builder := range cfg.FuncTools {
		tool, err := builder.Build()
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}

	// LLM Model
	llmModel, err := cfg.LLMAdapter.Model()
	if err != nil {
		return nil, err
	}

	// LLM Agent
	llmAgent, err := llmagent.New(llmagent.Config{
		Name:                 cfg.Name,
		Model:                llmModel,
		Description:          cfg.Description,
		Instruction:          cfg.Instruction,
		Tools:                tools,
		Toolsets:             toolsets,
		BeforeToolCallbacks:  cfg.ToolHooks.Before,
		AfterToolCallbacks:   cfg.ToolHooks.After,
		BeforeModelCallbacks: cfg.ModelHooks.Before,
		AfterModelCallbacks:  cfg.ModelHooks.After,
	})
	if err != nil {
		return nil, err
	}
	return llmAgent, nil
}

type AgentToolConfig struct {
	Name        string
	Description string
	Instruction string
	LLMAdapter  LLMAdapter
	MCPAgents   []*MCPAgent
	FuncAgents  []*FuncAgent
	ToolHooks   ToolCallback
	ModelHooks  ModelCallback
}

func NewMultiToolAgent(cfg *AgentToolConfig) (agent.Agent, error) {
	// LLM Model
	llmModel, err := cfg.LLMAdapter.Model()
	if err != nil {
		return nil, err
	}

	// MCP Tools
	tools := make([]tool.Tool, 0, len(cfg.MCPAgents))
	for _, v := range cfg.MCPAgents {
		tool, _err := NewMCPTool(llmModel, v)
		if _err != nil {
			return nil, _err
		}
		tools = append(tools, tool)
	}

	// Func Tools
	for _, v := range cfg.FuncAgents {
		tool, _err := NewFuncTool(llmModel, v)
		if _err != nil {
			return nil, _err
		}
		tools = append(tools, tool)
	}

	// LLM Agent
	llmAgent, err := llmagent.New(llmagent.Config{
		Name:                 cfg.Name,
		Model:                llmModel,
		Description:          cfg.Description,
		Instruction:          cfg.Instruction,
		Tools:                tools,
		BeforeToolCallbacks:  cfg.ToolHooks.Before,
		AfterToolCallbacks:   cfg.ToolHooks.After,
		BeforeModelCallbacks: cfg.ModelHooks.Before,
		AfterModelCallbacks:  cfg.ModelHooks.After,
	})
	if err != nil {
		return nil, err
	}
	return llmAgent, nil
}
