package openai

import (
	"strings"
	"testing"
)

func TestNormalizeToolCallID(t *testing.T) {
	model := &openaiModel{}

	short := "call_123"
	if got := model.normalizeToolCallId(short); got != short {
		t.Fatalf("short ID changed: got %q, want %q", got, short)
	}

	long := strings.Repeat("a", maxToolCallIdLength+1)
	first := model.normalizeToolCallId(long)
	second := model.normalizeToolCallId(long)
	if first != second {
		t.Fatalf("normalization is not deterministic: %q != %q", first, second)
	}
	if len(first) != maxToolCallIdLength {
		t.Fatalf("normalized ID length = %d, want %d", len(first), maxToolCallIdLength)
	}
	if !strings.HasPrefix(first, "tc_") {
		t.Fatalf("normalized ID %q lacks tc_ prefix", first)
	}
}
