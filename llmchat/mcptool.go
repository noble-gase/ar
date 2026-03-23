package llmchat

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/noble-gase/ne/helper"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/agenttool"
	"google.golang.org/adk/tool/mcptoolset"
	"google.golang.org/genai"
)

type MCPAgent struct {
	Name string
	// Endpoint is the URL of MCP server based on Streamable HTTP.
	Endpoint        string
	Description     string
	Instruction     string
	MaxOutputTokens int
	// LLMAdapter specifies the model for MCP-Agent, if not specified, the root agent model will be used.
	LLMAdapter LLMAdapter
}

func NewMCPTool(rootModel model.LLM, cfg *MCPAgent) (tool.Tool, error) {
	// MCP Toolset (Streamable HTTP)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   cfg.Endpoint,
		HTTPClient: helper.NewHttpClient(),
	}
	toolset, err := mcptoolset.New(mcptoolset.Config{
		Transport: transport,
	})
	if err != nil {
		return nil, err
	}

	agentCfg := llmagent.Config{
		Name:        cfg.Name,
		Model:       rootModel,
		Description: cfg.Description,
		Instruction: cfg.Instruction,
		Toolsets:    []tool.Toolset{toolset},
		GenerateContentConfig: &genai.GenerateContentConfig{
			MaxOutputTokens: int32(cfg.MaxOutputTokens),
		},
	}
	if cfg.LLMAdapter != nil {
		llmModel, _err := cfg.LLMAdapter.Model()
		if _err != nil {
			return nil, _err
		}
		agentCfg.Model = llmModel
	}

	// LLM Agent
	llmAgent, err := llmagent.New(agentCfg)
	if err != nil {
		return nil, err
	}
	return agenttool.New(llmAgent, nil), nil
}
