package llmchat

import (
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// ToolBuilder 是构造工具的接口。
type ToolBuilder interface {
	Build() (tool.Tool, error)
}

// FuncTool 构造一个执行函数的工具。
type FuncTool[TArgs, TResult any] struct {
	Name        string
	Description string

	// Handler 执行工具。TArgs 由模型给出的参数解码而来，TResult 作为工具结果
	// 返回给模型。
	//
	// TResult 必须是「对象」类型，即 struct 或 map[string]any。工具结果会被
	// 序列化为一个 JSON 对象返回给模型；若用 string、int、slice 等非对象类型，
	// 无法表示成 JSON object，会导致构建或调用失败。需要返回单个值时，请包一层
	// 结构体，例如 struct{ Result string `json:"result"` } 或 map[string]any。
	Handler functiontool.Func[TArgs, TResult]

	// RequireConfirmation 为 true 时，这个工具每次执行前都要发起人工确认（HITL）。
	RequireConfirmation bool

	// RequireConfirmationProvider 动态决定是否需要确认。签名必须是 func(TArgs) bool，
	// 设置后优先级高于 RequireConfirmation。
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
