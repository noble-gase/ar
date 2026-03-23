package llmchat

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gomod.sunmi.com/gomoddepend/golib/helper"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/mcptoolset"
	"google.golang.org/genai"
)

type NormalConfig struct {
	Name            string
	Description     string
	Instruction     string
	LLMAdapter      LLMAdapter
	MCPServers      []string
	FuncTools       []FuncToolBuilder
	MaxOutputTokens int
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
		tool, err := builder.build()
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
		Name:        cfg.Name,
		Model:       llmModel,
		Description: cfg.Description,
		Instruction: cfg.Instruction,
		Tools:       tools,
		Toolsets:    toolsets,
		GenerateContentConfig: &genai.GenerateContentConfig{
			MaxOutputTokens: int32(cfg.MaxOutputTokens),
		},
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
		tool, err := NewMCPTool(llmModel, v)
		if err != nil {
			return nil, err
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
		Name:        cfg.Name,
		Model:       llmModel,
		Description: cfg.Description,
		Instruction: cfg.Instruction,
		Tools:       tools,
	})
	if err != nil {
		return nil, err
	}
	return llmAgent, nil
}
