package llmchat

import (
	"context"

	"github.com/noble-gase/ar/model/anthropic"
	"github.com/noble-gase/ar/model/openai"
	"google.golang.org/adk/model"
	"google.golang.org/adk/model/gemini"
	"google.golang.org/genai"
)

// LLMAdapter is an interface that provides a method to get a model.LLM
type LLMAdapter interface {
	Model() (model.LLM, error)
}

// OpenAI is an adapter for the OpenAI compatible model
type OpenAI struct {
	Config openai.Config
}

func (o *OpenAI) Model() (model.LLM, error) {
	return openai.NewModel(o.Config), nil
}

// Anthropic is an adapter for the Anthropic compatible model
type Anthropic struct {
	Config anthropic.Config
}

func (a *Anthropic) Model() (model.LLM, error) {
	return anthropic.NewModel(a.Config), nil
}

// Gemini is an adapter for the Gemini compatible model
type Gemini struct {
	ModelName    string
	ClientConfig genai.ClientConfig
}

func (g *Gemini) Model() (model.LLM, error) {
	return gemini.NewModel(context.Background(), g.ModelName, &g.ClientConfig)
}

// VertexAI is an adapter for the VertexAI compatible model
type VertexAI struct {
	ModelName    string
	ClientConfig genai.ClientConfig
}

func (v *VertexAI) Model() (model.LLM, error) {
	return gemini.NewModel(context.Background(), v.ModelName, &v.ClientConfig)
}
