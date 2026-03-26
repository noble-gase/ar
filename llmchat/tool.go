package llmchat

import (
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// ToolBuilder is an interface that builds a tool.
type ToolBuilder interface {
	Build() (tool.Tool, error)
}

// FuncTool builds a tool that executes a function.
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
