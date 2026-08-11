package llmchat

import (
	"context"

	"github.com/noble-gase/argon/model/anthropic"
	"github.com/noble-gase/argon/model/openai"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/genai"
)

// LLMAdapter 是提供 model.LLM 的接口
type LLMAdapter interface {
	Model() (model.LLM, error)
}

// OpenAI 是 OpenAI 兼容模型的适配器
type OpenAI struct {
	Config openai.Config
}

func (o *OpenAI) Model() (model.LLM, error) {
	return openai.NewModel(o.Config), nil
}

// Anthropic 是 Anthropic 兼容模型的适配器
type Anthropic struct {
	Config anthropic.Config
}

func (a *Anthropic) Model() (model.LLM, error) {
	return anthropic.NewModel(a.Config), nil
}

// Gemini 是 Gemini 兼容模型的适配器
type Gemini struct {
	ModelName    string
	ClientConfig genai.ClientConfig
}

func (g *Gemini) Model() (model.LLM, error) {
	return gemini.NewModel(context.Background(), g.ModelName, &g.ClientConfig)
}

// VertexAI 是 VertexAI 兼容模型的适配器
type VertexAI struct {
	ModelName    string
	ClientConfig genai.ClientConfig
}

func (v *VertexAI) Model() (model.LLM, error) {
	return gemini.NewModel(context.Background(), v.ModelName, &v.ClientConfig)
}
