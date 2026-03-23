package llmchat

import (
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/agenttool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

type FuncToolBuilder interface {
	build() (tool.Tool, error)
}

type FuncTool[TArgs, TResults any] struct {
	Name        string
	Description string
	Handler     functiontool.Func[TArgs, TResults]
}

func (f *FuncTool[TArgs, TResults]) build() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        f.Name,
		Description: f.Description,
	}, f.Handler)
}

type FuncAgent struct {
	Name            string
	Description     string
	Instruction     string
	MaxOutputTokens int
	Tools           []FuncToolBuilder
	// LLMAdapter specifies the model for the Func-Agent, if not specified, the root agent model will be used.
	LLMAdapter LLMAdapter
}

func NewFuncTool(rootModel model.LLM, cfg *FuncAgent) (tool.Tool, error) {
	tools := make([]tool.Tool, 0, len(cfg.Tools))
	for _, v := range cfg.Tools {
		t, err := v.build()
		if err != nil {
			return nil, err
		}
		tools = append(tools, t)
	}

	agentCfg := llmagent.Config{
		Name:        cfg.Name,
		Model:       rootModel,
		Description: cfg.Description,
		Instruction: cfg.Instruction,
		Tools:       tools,
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
