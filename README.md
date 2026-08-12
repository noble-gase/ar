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
	"context"
	"os/signal"
	"syscall"

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

	if err := assistant.Start(); err != nil {
		panic(err)
	}

	// 阻塞到收到停止信号，Stop 会等在途消息跑完
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
}
```

> `Bot` 是一次性的：`Stop` 之后不能再 `Start`（会返回错误），需要继续服务请新建实例。

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

### GraphAgent（图工作流）

`GraphAgent` 支持条件分支、扇出与扇入（`workflow.JoinNode`）。运行状态持久化在 session 中，节点可以暂停并在后续轮次凭 `InterruptID` 精确恢复，因此**不会像上面三种 workflow 那样重复执行前置节点**。

边由 `Edges` 回调组装：Agent 节点用 `g.Agent`（模型在此注入，并自动登记为 SubAgent 以便 runner 解析事件作者），其余节点直接用 ADK 原生的 `workflow.NewFunctionNode` / `NewEmittingFunctionNode` / `NewJoinNode`，再用 `workflow.Chain` / `workflow.Concat` / `workflow.NewEdgeBuilder` 连边。

下面是一个「起草 → 人工审核 → 并行润色 → 汇总发布」的完整例子，覆盖条件分支、退回重写的环、扇出与扇入：

```
   START → draft → review → route ─┬─ "approved" ─→ polish ─┬→ gather → publish
                     ↑             │               seo    ──┘
                     │             └─ "rejected" ─→ draft
```

```go
root := &llmchat.GraphAgent{
	Name:        "article",
	Description: "起草、人工审核、并行润色后汇总发布",
	Edges: func(g *llmchat.Graph) []workflow.Edge {
		// Agent 节点：模型由 GraphAgent 注入，节点名即 Agent 名
		draft := g.Agent(&llmchat.NormalAgent{
			Name:        "draft",
			Instruction: "根据用户需求写一篇初稿",
		}, workflow.NodeConfig{})

		// HITL 节点：发出 RequestInput 后返回 ErrNodeInterrupted 暂停整个图，
		// 用户下一轮的回复会作为后继节点（route）的输入送进来
		review := workflow.NewEmittingFunctionNode[any, any]("review",
			func(ctx agent.Context, _ any, emit func(*session.Event) error) (any, error) {
				err := emit(workflow.NewRequestInputEvent(ctx, session.RequestInput{
					// InterruptID 必须每次唯一，否则新一轮的提问会被当成已回答
					InterruptID: "review-" + uuid.NewString(),
					Message:     "初稿已生成，回复 approved 发布，其他内容退回重写",
				}))
				if err != nil {
					return nil, err
				}
				return nil, workflow.ErrNodeInterrupted
			},
			workflow.NodeConfig{},
		)

		// 路由节点：把人工回复翻译成路由标记。Routes 决定走哪条边，
		// Output 决定传给后继节点的输入
		route := workflow.NewEmittingFunctionNode[string, any]("route",
			func(ctx agent.Context, reply string, emit func(*session.Event) error) (any, error) {
				ev := session.NewEvent(ctx, ctx.InvocationID())
				if strings.TrimSpace(reply) == "approved" {
					ev.Routes = []string{"approved"}
				} else {
					ev.Routes = []string{"rejected"}
				}
				ev.Output = reply
				return nil, emit(ev)
			},
			workflow.NodeConfig{},
		)

		polish := g.Agent(&llmchat.NormalAgent{
			Name:        "polish",
			Instruction: "润色文字表达",
		}, workflow.NodeConfig{})
		seo := g.Agent(&llmchat.NormalAgent{
			Name:        "seo",
			Instruction: "补充 SEO 关键词",
		}, workflow.NodeConfig{})

		// 扇入屏障：等齐所有前驱后，输出 map[前驱节点名]该节点输出
		gather := workflow.NewJoinNode("gather")

		publish := workflow.NewFunctionNode("publish",
			func(_ agent.Context, parts map[string]any) (string, error) {
				return fmt.Sprintf("%v\n\n%v", parts["polish"], parts["seo"]), nil
			},
			workflow.NodeConfig{},
		)

		eb := workflow.NewEdgeBuilder()
		eb.AddRoute(route, polish, workflow.StringRoute("approved"))
		eb.AddRoute(route, seo, workflow.StringRoute("approved"))
		eb.AddRoute(route, draft, workflow.StringRoute("rejected"))
		eb.AddFanIn(gather, polish, seo)
		eb.Add(gather, publish)

		return workflow.Concat(
			workflow.Chain(workflow.Start, draft, review, route),
			eb.Build(),
		)
	},
}
```

几个连边规则：

- **环必须带条件**：`route → draft` 这条退回边带了 `StringRoute("rejected")`，无条件环会被 `ErrUnconditionalCycle` 拒绝
- **扇入边必须无条件**：`JoinNode` 会等齐所有声明的前驱，被路由跳过的前驱永远不会触发，所以 `AddFanIn` 的入边不能带 `Route`；扇出到 `polish`/`seo` 的两条边用同一个 route，保证要么都激活要么都不激活
- **非 `JoinNode` 的扇入尚不支持**（`ErrUnsupportedFanIn`），多前驱汇聚一律走 `JoinNode`

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
		Approve:    dingtalk.ConfirmAction{Value: "approve"},
		Reject:     dingtalk.ConfirmAction{Value: "reject"},
	},
	Timeout:       time.Hour,        // 单次运行（LLM + 工具调用）总时长上限，默认 1 小时
	ShutdownGrace: 15 * time.Second, // 停机时让在途消息自然跑完的宽限期，默认 15 秒
}
```

`Stop` 会先停止接收，再等在途消息自然结束；超过 `ShutdownGrace` 才发出取消。有上限的只是这段自然排空——取消之后 `Stop` 仍会等 handler 真正退出，因为 Go 杀不死 goroutine，提前返回并关闭卡片客户端只会让残留任务写已关闭的资源。完全忽略 context 的任务只能由进程管理器（如 Kubernetes 的 `terminationGracePeriodSeconds`）兜底，把它配得比 `ShutdownGrace` 长。

### 人工输入（图工作流）

`GraphAgent` 的节点用 `workflow.NewRequestInputEvent` + `ErrNodeInterrupted` 暂停时，走的是另一条通道：确认是「同意/拒绝」，人工输入是「自由文本回答」。用 `llmchat.RequestInputOf` 识别，用 `Reply` / `ReplyConversation` 恢复：

```go
for event, err := range events {
	if err != nil {
		return err
	}
	if in, ok := llmchat.RequestInputOf(event); ok {
		// in.InterruptId / in.Message / in.Payload / in.ResponseSchema
		// 把 in.Message 展示给用户，拿到回答后送回图中，回答会作为暂停节点的后继节点输入。
		// 节点声明了 ResponseSchema 时用 ReplyPayload 把文本转成结构化数据
		payload := llmchat.ReplyPayload(answer, in.ResponseSchema)
		resumed, err := chat.ReplyConversation(ctx, userId, conversationId, in.InterruptId, payload)
		_, _ = resumed, err
		continue
	}
}
```

回答不符合 `ResponseSchema` 时节点会保持 waiting，`llmchat.IsRejectedReply(err)` 可以识别这种情况并让用户重答。

钉钉渠道已内置，无需额外配置卡片模板：

- 节点提问写回当前卡片，用户的**下一条聊天消息**即作为回答送回图中
- 扇出会让多个节点同一轮暂停，此时按发起顺序逐个提问，一条消息回答一个，卡片提示还剩几个
- 节点要求结构化回答时，卡片会附上 schema 并提示以 JSON 回复；回答不合要求时同一个问题会被再问一次
- 回复 `/cancel` 或 `取消` 可放弃全部待回答问题与待处理的工具确认（会重置当前自动会话），关键词通过 `dingtalk.Config.CancelKeywords` 自定义

> **消息路由**：一次只走一条路。有工具确认挂着时，普通消息不会另起一轮，而是提示先在确认卡上做决定或回复「取消」——否则确认卡指向的执行会和新一轮同时挂在一个会话上。图工作流的待答问题则相反：下一条消息就是回答。两者都由 `Chat.Pending` 一次性从会话历史重建，不看渠道侧的记录。

> **状态一致性**：待回答问题与待确认的工具调用**只存在于 ADK 会话中**，渠道侧不维护任何副本——每轮消息都用 `Chat.Pending` 从会话历史重建（待答问题与待确认调用共用一次加载）。因此不存在缓存过期、队列与会话不一致的问题：确认记录丢了不会放行新一轮，记录残留也不会把用户挡在门外。会话状态读取失败时会提示稍后重试，而不会把回答误当成新提问、也不会显示成流程已结束。
>
> **多实例部署**：聊天消息与确认卡片回调都经同一把 Redis 分布式锁串行化（按用户加锁、持锁期间自动续期、只释放自己持有的锁），不会出现两个 runner 并发驱动同一会话。Redis 在这里只保存两样东西：确认卡片的 `outTrackId → callId` 映射，以及这把用户锁。两者丢失都不影响正确性，只影响体验。

## 会话

### 自动会话

钉钉等不管理对话 ID 的渠道使用 `Ask` / `Confirm`。每个用户有一条「当前会话」指针记录，跨自然日自动轮换到新会话；轮换是单调的（只向日期更大的方向轮换），时区配置不一致或时钟回拨不会让实例间互相踩踏。日界时区默认 `time.Local`，多实例部署必须通过 `session.WithLocation` 显式统一。并发调用与崩溃重试通过指针表的条件写收敛到同一个会话。`ResetAutomatic` 把指针换成全新 ID 并尽力删除旧会话（删除失败只留下无法寻址的孤儿，不影响正确性）。

`Ask` / `Reply` 会返回本次运行所在的会话。需要把状态存到进程外（例如一张等待点击的确认卡片）时必须记下它，之后原样传回 `Confirm`——不要在恢复时重新解析。`Confirm` 恢复前会核对会话与会话历史，三种失效各自有明确的错误：

```go
conversationId, events, err := chat.Ask(ctx, userId, text)

// 用户点击确认按钮后，带上当初那个会话
events, err = chat.Confirm(ctx, userId, conversationId, callId, true, nil)
switch {
case errors.Is(err, llmchat.ErrConversationChanged):
    // 会话已轮换（跨日或被重置），这次确认不再可恢复，提示用户重新发起
case errors.Is(err, llmchat.ErrAlreadyConfirmed):
    // 重复点击，工具不会被执行第二次
case errors.Is(err, llmchat.ErrConfirmationNotFound):
    // 防御性兜底：会话匹配但历史里查无此确认（如调用方传错 callId）
}
```

> 判定依据始终是 ADK 会话，不是渠道侧的缓存：缓存清理失败不会造成重复执行。跨日轮换与 `ResetAutomatic` 都会让指针指向全新的会话 ID，因此两种失效统一由 `ErrConversationChanged` 拦截。

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
