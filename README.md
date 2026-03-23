# 氩-Ar

[氩-Ar] AI智能助手开发库

> 当前仅支持「钉钉」

## Install

```shell
go get github.com/noble-gase/ar
```

## Usage

### Normal

<details>
<summary>点击展开</summary>

```go
package main

import (
	"github.com/achetronic/adk-utils-go/genai/openai"
	"github.com/noble-gase/ar"
	"github.com/noble-gase/ar/llmchat"
)

func main() {
	// llmchat
	cfg := &llmchat.NormalConfig{
		Name: "iota",
		Description: "IOTA智能助手",
		Instruction: `你是一个企业内部智能助手。
## 基本规则
- 用中文回答，简洁、准确，使用 Markdown 格式
- 列表数据，请使用 Markdown 表格输出展示
- 不要凭自身知识回答问题，必须通过工具获取正确的信息
- 如果用户的问题与工具列表范围无关，请告知用户无法处理
- 遇到工具不能处理的问题，请如实告知，并让用户找「盛辉」确认`,
		LLMAdapter: &llmchat.OpenAI{
			Config: openai.Config{
				APIKey: "sk-xxxxxx",
				BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1",
				ModelName: "glm-5",
			},
		},
		MCPServers: []string{"http://localhost:8080/mcp/iotlink"},
		MaxOutputTokens: 1024,
	}
	chat, err := ar.NewNormalChat("IOTA-Agent", db, redis, cfg)
	if err != nil {
		panic(err)
	}

	// dingtalk
	assistant, err := ar.NewAssistant("clientId", "clientSecret", "cardTemplateId", redis, chat)
	if err != nil {
		panic(err)
	}
	defer assistant.Stop()

	assistant.Start()
}
```

</details>

### AgentTool

<details>
<summary>点击展开</summary>

```go
package main

import (
	"github.com/achetronic/adk-utils-go/genai/openai"
	"github.com/noble-gase/ar"
	"github.com/noble-gase/ar/llmchat"
)

func main() {
	// llmchat
	chatCfg := llmchat.AgentToolConfig{
		Name: "iota",
		Description: "IOTA智能助手",
		Instruction: `你是一个企业内部智能助手，负责理解用户意图并将任务分发给合适的 Agent 工具。
## 基本规则
- 不要凭自身知识回答问题，必须通过 Agent 工具获取正确的信息`,
		LLMAdapter: &llmchat.OpenAI{
			Config: openai.Config{
				APIKey: "sk-xxxxxx",
				BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1",
				ModelName: "glm-5",
			},
		},
		MCPAgents: []llmchat.MCPAgent{
			{
				Name: "iotlink"
				Endpoint: "http://localhost:8080/mcp/iotlink",
				Description: "联接平台相关工具",
				Instruction: `你是一个物联网「联接平台」相关的工具集合，你可以回答 MQTT 连接相关的问题。
## 基本规则
- 用中文回答，简洁、准确，使用 Markdown 格式
- 列表数据，请使用 Markdown 表格输出展示
- 不要凭自身知识回答问题，必须通过工具获取正确的信息
- 如果用户的问题与工具列表范围无关，请告知用户无法处理
- 遇到工具不能处理的问题，请如实告知，并让用户找「盛辉」确认`,
				MaxOutputTokens: 1024,
			},
		},
	}
	chat, err := ar.NewAgentToolChat("IOTA-Agent", db, redis, cfg)
	if err != nil {
		panic(err)
	}

	// dingtalk
	assistant, err := ar.NewAssistant("clientId", "clientSecret", "cardTemplateId", redis, chat)
	if err != nil {
		panic(err)
	}
	defer assistant.Stop()

	assistant.Start()
}
```

</details>
