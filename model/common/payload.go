package common

import "encoding/json"

var emptyJSONObject = json.RawMessage(`{}`)

// MarshalPayload 把工具入参或工具响应的 map 编码成上线格式。
// nil/空 编码成 "{}" 而不是 "null"：严格的 OpenAI 兼容解析器（vLLM/llama.cpp
// 上的 Qwen）在期望对象的位置收到 "null" 会直接拒绝。已经编码好的
// json.RawMessage 原样透传。入参不会被修改。
func MarshalPayload(payload any) (json.RawMessage, error) {
	if payload == nil {
		return emptyJSONObject, nil
	}

	if raw, ok := payload.(json.RawMessage); ok {
		if len(raw) == 0 {
			return emptyJSONObject, nil
		}
		return raw, nil
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || string(data) == "null" {
		return emptyJSONObject, nil
	}
	return data, nil
}
