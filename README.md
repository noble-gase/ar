# argon-go

[![golang](https://img.shields.io/badge/Language-Go-green.svg?style=flat)](https://golang.org)
[![pkg.go.dev](https://img.shields.io/badge/dev-reference-007d9c?logo=go&logoColor=white&style=flat)](https://pkg.go.dev/github.com/noble-gase/argon)
[![MIT](http://img.shields.io/badge/license-MIT-brightgreen.svg)](http://opensource.org/licenses/MIT)

[氩-Argon] AI 智能助手开发库｜Assistant Development Kit (ADK) for Go

## Install

```shell
go get github.com/noble-gase/argon
```

## Usage

### Normal

<details>
<summary>点击展开</summary>

```go
package main

import (
	"github.com/noble-gase/argon"
	"github.com/noble-gase/argon/channel/dingtalk"
	"github.com/noble-gase/argon/llmchat"
	"github.com/noble-gase/argon/model/openai"
)

func main() {
	// agent
	agent := &llmchat.NormalAgent{
		Name: "iota",
		Description: "IOTA智能助手",
		Instruction: `你是一个企业内部智能助手。
## 基本规则
- 用中文回答，简洁、准确，使用 Markdown 格式
- 列表数据，请使用 Markdown 表格输出展示
- 不要凭自身知识回答问题，必须通过工具获取正确的信息
- 如果用户的问题与工具列表范围无关，请告知用户无法处理
- 结果必须全部显示，不要省略字段，更不要使用 ... 省略内容
- 遇到工具不能处理的问题，请如实告知，并让用户找「xxx」确认`,
		LLMAdapter: &llmchat.OpenAI{
			Config: openai.Config{
				APIKey: "sk-xxxxxxxxx",
				BaseURL: "https://api.deepseek.com",
				ModelName: "deepseek-v4-pro",
			},
		},
		Skills: []string{"./skills"},
	}

	// llmchat
	chat, err := argon.NewLLMChat("IOTA-Agent", db, redis, agent)
	if err != nil {
		panic(err)
	}

	// dingtalk
	cfg := &dingtalk.Config{
		ClientId: "clientId",
		ClientSecret: "clientSecret",
		CardTemplateId: "xxxxxx.schema",
	}
	assistant, err := argon.NewDingTalkAssistant(cfg, redis, chat)
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
	"github.com/noble-gase/argon"
	"github.com/noble-gase/argon/channel/dingtalk"
	"github.com/noble-gase/argon/llmchat"
	"github.com/noble-gase/argon/model/openai"
)

func main() {
	// agent
	agent := &llmchat.AgentTool{
		Name: "iota",
		Description: "IOTA智能助手",
		Instruction: `你是一个企业内部智能助手，负责理解用户意图并将任务分发给合适的 Agent 工具。
## 基本规则
- 不要凭自身知识回答问题，必须通过 Agent 工具获取正确的信息
- 结果必须全部显示，不要省略字段，更不要使用 ... 省略内容`,
		LLMAdapter: &llmchat.OpenAI{
			Config: openai.Config{
				APIKey: "sk-xxxxxxxxx",
				BaseURL: "https://api.deepseek.com",
				ModelName: "deepseek-v4-flash",
			},
		},
		Tools: []llmchat.AgentBuilder{
			&llmchat.SkillAgent{
				Name: "iotlink"
				Skills: []string{"./skills"},
				Description: "联接平台相关工具",
				Instruction: `你是一个物联网「联接平台」相关的工具集合，你可以回答 MQTT 连接相关的问题。
## 基本规则
- 用中文回答，简洁、准确，使用 Markdown 格式
- 列表数据，请使用 Markdown 表格输出展示
- 不要凭自身知识回答问题，必须通过工具获取正确的信息
- 如果用户的问题与工具列表范围无关，请告知用户无法处理
- 遇到工具不能处理的问题，请如实告知，并让用户找「xxx」确认`,
			},
		},
	}

	// llmchat
	chat, err := argon.NewLLMChat("IOTA-Agent", db, redis, agent)
	if err != nil {
		panic(err)
	}

	// dingtalk
	cfg := &dingtalk.Config{
		ClientId: "clientId",
		ClientSecret: "clientSecret",
		CardTemplateId: "xxxxxx.schema",
	}
	assistant, err := argon.NewDingTalkAssistant(cfg, redis, chat)
	if err != nil {
		panic(err)
	}
	defer assistant.Stop()

	assistant.Start()
}
```

</details>
