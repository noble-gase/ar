# argon-go

[![golang](https://img.shields.io/badge/Language-Go-green.svg?style=flat)](https://golang.org)
[![pkg.go.dev](https://img.shields.io/badge/dev-reference-007d9c?logo=go&logoColor=white&style=flat)](https://pkg.go.dev/github.com/noble-gase/argon)
[![MIT](http://img.shields.io/badge/license-MIT-brightgreen.svg)](http://opensource.org/licenses/MIT)

[氩-Argon] AI 智能助手开发库｜Assistant Development Kit (ADK) for Go

基于 [Google ADK for Go](https://pkg.go.dev/google.golang.org/adk/v2) 封装，提供开箱即用的 Agent 构建、工具编排、人工确认（HITL）、会话持久化与钉钉渠道接入。

## 特性

- **多模型适配**：OpenAI 兼容、Anthropic、Gemini、VertexAI
- **多种工具形态**：函数工具、Skills 目录、MCP 服务（Streamable HTTP）、子 Agent
- **人工确认（HITL）**：每个工具来源可独立配置确认策略，支持暂停与恢复
- **会话持久化**：基于 GORM，支持自动会话（按自然日轮转）与显式会话（网页版多对话）
- **渠道接入**：钉钉机器人，流式卡片输出 + 确认卡片

## Install

```shell
go get github.com/noble-gase/argon
```

## 快速开始

以钉钉助手为例：

```go
package main

import (
	"github.com/noble-gase/argon"
	"github.com/noble-gase/argon/channel/dingtalk"
	"github.com/noble-gase/argon/llmchat"
	"github.com/noble-gase/argon/model/openai"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
)

func main() {
	agent := &llmchat.NormalAgent{
		Name:        "iota",
		Description: "企业内部智能助手",
		Instruction: `你是一个企业内部智能助手。
## 基本规则
- 用中文回答，简洁、准确，使用 Markdown 格式
- 列表数据，请使用 Markdown 表格输出展示
- 不要凭自身知识回答问题，必须通过工具获取正确的信息
- 如果用户的问题与工具列表范围无关，请告知用户无法处理
- 结果必须全部显示，不要省略字段，更不要使用 ... 省略内容`,
		LLMAdapter: &llmchat.OpenAI{
			Config: openai.Config{
				APIKey:    "sk-xxxxxxxxx",
				BaseURL:   "https://api.deepseek.com",
				ModelName: "deepseek-chat",
			},
		},
		Skills: []llmchat.SkillSource{
			{Path: "./skills"},
		},
	}

	// 会话存储：连接生命周期由 dialector 的持有方负责
	db := mysql.Open("user:pass@tcp(127.0.0.1:3306)/argon?charset=utf8mb4&parseTime=True")

	chat, err := argon.NewLLMChat("iota", db, agent)
	if err != nil {
		panic(err)
	}

	// 钉钉渠道：redis 用于卡片投放状态
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})

	assistant, err := argon.NewDingTalkAssistant(&dingtalk.Config{
		ClientId:       "clientId",
		ClientSecret:   "clientSecret",
		CardTemplateId: "xxxxxx.schema",
	}, rdb, chat)
	if err != nil {
		panic(err)
	}
	defer assistant.Stop()

	assistant.Start()
}
```

只需要 Agent 实例而不需要会话与渠道时，使用 `argon.NewLLMAgent(builder)`。

## Agent

`llmchat.NormalAgent` 是通用 Agent 构建器，一个实例即可混用全部工具形态。它可以作为根 Agent、workflow 的子 Agent，也可以通过 `AgentTools` 被暴露为工具。

| 字段                                      | 说明                                                |
| ----------------------------------------- | --------------------------------------------------- |
| `Name` / `Description` / `Instruction`    | 名称、能力描述、系统提示词                          |
| `LLMAdapter`                              | 模型适配器；不设置则继承根 Agent 的模型             |
| `Tools`                                   | 函数工具（`FuncTool`）                              |
| `Skills`                                  | 从目录加载的 Skills 工具集                          |
| `MCPServers`                              | MCP 服务（Streamable HTTP）                         |
| `AgentTools`                              | 子 Agent 以工具形式暴露（agent-as-tool）            |
| `SubAgents`                               | 子 Agent 以 LLM 委派形式暴露（`transfer_to_agent`） |
| `AgentHooks` / `ToolHooks` / `ModelHooks` | Agent、工具、模型层的回调钩子                       |
| `OutputKey`                               | 仅用于 workflow 协作时传递输出                      |

### 模型适配器

```go
&llmchat.OpenAI{Config: openai.Config{APIKey: "...", BaseURL: "...", ModelName: "..."}}
&llmchat.Anthropic{Config: anthropic.Config{APIKey: "...", ModelName: "claude-sonnet-4-5-20250929"}}
&llmchat.Gemini{ModelName: "gemini-2.5-pro", ClientConfig: genai.ClientConfig{}}
&llmchat.VertexAI{ModelName: "gemini-2.5-pro", ClientConfig: genai.ClientConfig{}}
```

### 函数工具

`TResult` 必须是「对象」类型（struct 或 `map[string]any`），因为工具结果会被序列化为 JSON 对象返回给模型。需要返回单个值时请包一层结构体。

```go
type QueryArgs struct {
	DeviceId string `json:"deviceId"`
}

type QueryResult struct {
	Online bool   `json:"online"`
	Note   string `json:"note"`
}

agent.Tools = []llmchat.ToolBuilder{
	&llmchat.FuncTool[QueryArgs, QueryResult]{
		Name:        "query_device",
		Description: "查询设备在线状态",
		Handler: func(ctx agent.Context, args QueryArgs) (QueryResult, error) {
			return QueryResult{Online: true}, nil
		},
	},
}
```

### Skills

`Path` 指向的目录以「一级子目录 = 一个 skill」的方式组织：

```
skills/
  skill-1/
    SKILL.md
    assets/
  skill-2/
    SKILL.md
    references/
    scripts/
```

```go
agent.Skills = []llmchat.SkillSource{
	{Path: "./skills"},
}
```

### MCP 服务

```go
agent.MCPServers = []llmchat.MCPSource{
	{Endpoint: "https://mcp.example.com/mcp"},
}
```

### AgentTools（子 Agent 作为工具）

适合把不同领域的工具集分组，减少根 Agent 上下文中的工具声明数量。

<details>
<summary>点击展开示例</summary>

```go
agent := &llmchat.NormalAgent{
	Name:        "iota",
	Description: "企业内部智能助手",
	Instruction: `你是一个企业内部智能助手，负责理解用户意图并将任务分发给合适的 Agent 工具。
## 基本规则
- 不要凭自身知识回答问题，必须通过 Agent 工具获取正确的信息
- 结果必须全部显示，不要省略字段，更不要使用 ... 省略内容`,
	LLMAdapter: &llmchat.OpenAI{
		Config: openai.Config{
			APIKey:    "sk-xxxxxxxxx",
			BaseURL:   "https://api.deepseek.com",
			ModelName: "deepseek-chat",
		},
	},
	AgentTools: []llmchat.AgentToolSource{
		{
			Agent: &llmchat.NormalAgent{
				Name:        "iotlink",
				Description: "联接平台相关工具",
				Instruction: `你是一个物联网「联接平台」相关的工具集合，可以回答 MQTT 连接相关的问题。
## 基本规则
- 用中文回答，简洁、准确，使用 Markdown 格式
- 列表数据，请使用 Markdown 表格输出展示
- 不要凭自身知识回答问题，必须通过工具获取正确的信息`,
				Skills: []llmchat.SkillSource{
					{Path: "./skills"},
				},
			},
		},
	},
}
```

</details>

> **注意**：`AgentTools` 的确认粒度是「调用整个子 Agent」这一步。子 Agent 的**内部工具**运行在隔离的 sub-runner 中，其确认不会冒泡到顶层。若需要对内部某个具体工具做确认，请把该工具直接挂在 `NormalAgent` 的 `Tools` / `Skills` / `MCPServers` 上，或改用 `SubAgents`。

### SubAgents（LLM 委派）

与 `AgentTools` 不同，`SubAgents` 通过 `transfer_to_agent` 委派，子 Agent 与父 Agent 共享同一个 runner/session，因此**子 Agent 内部工具的人工确认可以正常冒泡并恢复**。同时子 Agent 的工具只在委派发生后才向模型声明，同样能节省上下文。

```go
agent.SubAgents = []llmchat.AgentBuilder{
	&llmchat.NormalAgent{
		Name:        "iotlink",
		Description: "联接平台相关工具",
		Instruction: "...",
		Tools:       []llmchat.ToolBuilder{ /* ... */ },
	},
}
```

### Workflow Agents

`SequentialAgent`、`LoopAgent`、`ParallelAgent` 用于确定性编排，通过 `OutputKey` 在步骤间传递结果。

```go
root := &llmchat.SequentialAgent{
	Name: "pipeline",
	SubAgents: []llmchat.AgentBuilder{
		&llmchat.NormalAgent{Name: "collect", OutputKey: "raw"},
		&llmchat.NormalAgent{Name: "summarize"},
	},
}
```

> **人工确认限制**：workflow 的 `Run` 每次都从头迭代子 Agent，没有暂停点续跑机制。确认事件虽能冒泡出卡片，但恢复时会从第一个子 Agent 重新执行，中间步骤需要确认时前置步骤会被重复执行。`ParallelAgent` 下并发产生的多个确认更无法可靠路由。**需要稳定 HITL 的工具请放在单个 `NormalAgent` 上。**

## 人工确认（HITL）

每个工具来源都可以独立配置确认策略：

```go
// 函数工具
&llmchat.FuncTool[Args, Result]{
	RequireConfirmation: true,
	// 或动态决策，签名必须是 func(Args) bool，优先级高于 RequireConfirmation
	RequireConfirmationProvider: func(args Args) bool { return args.Danger },
}

// Skills / MCP / AgentTools 同理
llmchat.SkillSource{Path: "./skills", RequireConfirmation: true}
llmchat.MCPSource{Endpoint: "https://mcp.example.com/mcp", ConfirmationProvider: provider}
```

消费事件流时用 `llmchat.ConfirmationOf` 识别待确认事件，再调用 `Confirm` / `ConfirmConversation` 恢复执行：

```go
events, err := chat.AskConversation(ctx, userId, conversationId, text)
if err != nil {
	return err
}

for event, err := range events {
	if err != nil {
		return err
	}
	if c, ok := llmchat.ConfirmationOf(event); ok {
		// c.CallId / c.ToolName / c.Args / c.Hint / c.Payload
		// 展示给用户，拿到决策后恢复执行；最后一个 payload 参数可为 nil
		resumed, err := chat.ConfirmConversation(ctx, userId, conversationId, c.CallId, true, nil)
		_, _ = resumed, err
		continue
	}
	// 正常输出事件
}
```

钉钉渠道已内置确认卡片，通过 `dingtalk.Config.ConfirmCard` 配置模板与按钮取值规则：

```go
cfg := &dingtalk.Config{
	ClientId:       "clientId",
	ClientSecret:   "clientSecret",
	CardTemplateId: "xxxxxx.schema",
	ConfirmCard: &dingtalk.ConfirmCard{
		TemplateId: "yyyyyy.schema", // 留空则复用 CardTemplateId
		ParamKey:   "action",
		Approve:    dingtalk.ConfirmAction{Value: "approve", Params: map[string]string{"status": "approved"}},
		Reject:     dingtalk.ConfirmAction{Value: "reject", Params: map[string]string{"status": "rejected"}},
	},
	Timeout: time.Hour, // 单次运行（LLM + 工具调用）总时长上限，默认 1 小时
}
```

## 会话

### 自动会话

钉钉等不管理对话 ID 的渠道使用 `Ask` / `Confirm`。会话 ID 由（应用, 用户, 自然日）确定性派生，跨自然日自动轮转到新会话（日界按服务器本地时区 `time.Local` 计算）。并发调用与崩溃重试会天然收敛到同一个会话。

```go
events, err := chat.Ask(ctx, userId, text)
events, err = chat.Confirm(ctx, userId, callId, true, nil)
```

### 显式会话

网页版应显式管理用户和对话，不依赖空闲时间自动切换：

```go
// 用户点击「新建对话」，conversationId 返回给前端保存
conversationId, err := chat.NewConversation(ctx, userId)

// 也可以由调用方指定 ID
err = chat.CreateConversation(ctx, userId, "my-conversation-id")

// 后续消息必须同时携带用户 ID 和对话 ID
events, err := chat.AskConversation(ctx, userId, conversationId, text)

// 读取会话（含事件）与产品层元数据
sess, err := chat.GetConversation(ctx, userId, conversationId)
meta, err := chat.GetConversationMeta(ctx, userId, conversationId)

// 重命名标题，不影响列表排序
err = chat.RenameConversation(ctx, userId, conversationId, "MQTT 排障")

// 游标分页查询与删除
page, err := chat.ListConversations(ctx, userId, "", 20)
next, err := chat.ListConversations(ctx, userId, page.NextCursor, 20)
err = chat.DeleteConversation(ctx, userId, conversationId)
```

`AskConversation` 只接受已存在且属于该用户的对话，并会把该对话移到列表前部。

### 存储模型

`conversations` 元数据表是会话对用户**是否可见的唯一依据**，ADK session 只保存运行状态和事件。两步操作的顺序保证了失败时的一致性：

- **创建**：先建 ADK session，再写元数据。失败时至多留下一个不可见的 ADK session，不会出现「列表里有、打开却 404」。
- **删除**：先删元数据让会话立即不可见，再删 ADK session。第二步失败只会留下用户无法寻址的孤儿会话，不会让已删除的会话重新出现在列表里。

约束：应用名 ≤ 32 字符，用户 ID 与对话 ID ≤ 64 字符，标题 ≤ 128 字符。

## License

[MIT](LICENSE)
