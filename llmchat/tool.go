package llmchat

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/noble-gase/ne/helper"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/agenttool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/adk/tool/mcptoolset"
)

type ToolBuilder interface {
	Build() (tool.Tool, error)
}

type FuncTool[TArgs, TResults any] struct {
	Name        string
	Description string
	Handler     functiontool.Func[TArgs, TResults]
}

func (f *FuncTool[TArgs, TResults]) Build() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        f.Name,
		Description: f.Description,
	}, f.Handler)
}

type FuncAgent struct {
	Name        string
	Description string
	Instruction string
	Tools       []ToolBuilder
	ToolHooks   ToolCallback
	ModelHooks  ModelCallback
	// LLMAdapter specifies the model for Func-Agent, if not specified, the root agent model will be used.
	LLMAdapter LLMAdapter
}

func NewFuncTool(rootModel model.LLM, cfg *FuncAgent) (tool.Tool, error) {
	tools := make([]tool.Tool, 0, len(cfg.Tools))
	for _, v := range cfg.Tools {
		t, err := v.Build()
		if err != nil {
			return nil, err
		}
		tools = append(tools, t)
	}

	agentCfg := llmagent.Config{
		Name:                 cfg.Name,
		Model:                rootModel,
		Description:          cfg.Description,
		Instruction:          cfg.Instruction,
		Tools:                tools,
		BeforeToolCallbacks:  cfg.ToolHooks.Before,
		AfterToolCallbacks:   cfg.ToolHooks.After,
		BeforeModelCallbacks: cfg.ModelHooks.Before,
		AfterModelCallbacks:  cfg.ModelHooks.After,
	}
	if cfg.LLMAdapter != nil {
		// LLM Model
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

type MCPAgent struct {
	Name string
	// Endpoint is the URL of MCP server based on Streamable HTTP.
	Endpoint    string
	Description string
	Instruction string
	ToolHooks   ToolCallback
	ModelHooks  ModelCallback
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
		Name:                 cfg.Name,
		Model:                rootModel,
		Description:          cfg.Description,
		Instruction:          cfg.Instruction,
		Toolsets:             []tool.Toolset{toolset},
		BeforeToolCallbacks:  cfg.ToolHooks.Before,
		AfterToolCallbacks:   cfg.ToolHooks.After,
		BeforeModelCallbacks: cfg.ModelHooks.Before,
		AfterModelCallbacks:  cfg.ModelHooks.After,
	}
	if cfg.LLMAdapter != nil {
		// LLM Model
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
