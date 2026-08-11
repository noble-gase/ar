package dingtalk

import (
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/noble-gase/argon/llmchat"
)

// inputNote 渲染提问文案，remaining 是含本题在内的剩余待回答数量。
func inputNote(prior string, in *llmchat.RequestInput, remaining int) string {
	note := prior
	if strings.TrimSpace(note) != "" {
		note += "\n\n"
	}

	prompt := strings.TrimSpace(in.Message)
	if prompt == "" {
		prompt = "请补充信息"
	}
	note += "> ❓ " + prompt
	if payload := formatJSONBlock(in.Payload); payload != "" {
		note += "\n\n附加信息：\n" + payload
	}
	note += schemaHint(in.ResponseSchema)

	note += "\n\n请直接回复消息继续。"
	if remaining > 1 {
		note += fmt.Sprintf("（还有 %d 个问题待回答）", remaining-1)
	}
	return note
}

// schemaHint 在节点要求结构化回答时附上 JSON 格式说明，否则返回空串。
func schemaHint(schema *jsonschema.Schema) string {
	if !structured(schema) {
		return ""
	}
	return "\n\n需要以 JSON 格式回复，要求：\n" + formatJSONBlock(schema)
}

// structured 表示节点要的是结构化数据，纯文本会被 schema 校验挡回来。
func structured(schema *jsonschema.Schema) bool {
	return schema != nil && schema.Type != "" && schema.Type != "string"
}
