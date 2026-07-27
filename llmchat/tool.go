package llmchat

import (
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// ToolBuilder is an interface that builds a tool.
type ToolBuilder interface {
	Build() (tool.Tool, error)
}

// FuncTool builds a tool that executes a function.
type FuncTool[TArgs, TResult any] struct {
	Name        string
	Description string

	// Handler executes the tool. TArgs is decoded from the model-provided
	// arguments, and TResult is returned to the model as the tool result.
	//
	// TResult 必须是「对象」类型，即 struct 或 map[string]any。工具结果会被
	// 序列化为一个 JSON 对象返回给模型；若用 string、int、slice 等非对象类型，
	// 无法表示成 JSON object，会导致构建或调用失败。需要返回单个值时，请包一层
	// 结构体，例如 struct{ Result string `json:"result"` } 或 map[string]any。
	Handler functiontool.Func[TArgs, TResult]

	// RequireConfirmation, when true, makes this tool always request a
	// Human-in-the-Loop (HITL) confirmation before execution.
	RequireConfirmation bool

	// RequireConfirmationProvider dynamically decides whether confirmation is
	// required. Its signature must be func(TArgs) bool and it takes precedence
	// over RequireConfirmation when set.
	RequireConfirmationProvider any
}

func (f *FuncTool[TArgs, TResult]) Build() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:                        f.Name,
		Description:                 f.Description,
		RequireConfirmation:         f.RequireConfirmation,
		RequireConfirmationProvider: f.RequireConfirmationProvider,
	}, f.Handler)
}
