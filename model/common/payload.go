package common

import "encoding/json"

var emptyJSONObject = json.RawMessage(`{}`)

// MarshalPayload encodes a tool-call args or tool-response map for the wire.
// nil/empty become "{}", never "null": strict OpenAI-compatible parsers (Qwen on
// vLLM/llama.cpp) reject "null" where they expect an object. An already-encoded
// json.RawMessage passes through. The input is never mutated.
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
