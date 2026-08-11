package llmchat

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/workflow"
)

func TestIsRejectedReply(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil"},
		{name: "schema mismatch", err: workflow.ErrInvalidResumeResponse, want: true},
		{name: "wrapped schema mismatch", err: fmt.Errorf("resume: %w", workflow.ErrInvalidResumeResponse), want: true},
		{name: "unrelated", err: errors.New("connection reset")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRejectedReply(tt.err); got != tt.want {
				t.Errorf("IsRejectedReply() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReplyPayload(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		schema *jsonschema.Schema
		want   any
	}{
		{
			name:   "no schema passes text through",
			text:   "true",
			schema: nil,
			want:   "true",
		},
		{
			name:   "string schema passes text through",
			text:   "123",
			schema: &jsonschema.Schema{Type: "string"},
			want:   "123",
		},
		{
			name:   "boolean schema decodes",
			text:   "true",
			schema: &jsonschema.Schema{Type: "boolean"},
			want:   true,
		},
		{
			name:   "unparsable text falls through so the node reports it",
			text:   "yes please",
			schema: &jsonschema.Schema{Type: "object"},
			want:   "yes please",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReplyPayload(tt.text, tt.schema); got != tt.want {
				t.Errorf("ReplyPayload() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestReplyPayloadDecodesObject(t *testing.T) {
	got := ReplyPayload(`{"approved":true}`, &jsonschema.Schema{Type: "object"})

	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("ReplyPayload() = %#v, want a decoded object", got)
	}
	if m["approved"] != true {
		t.Errorf("ReplyPayload() approved = %v, want true", m["approved"])
	}
}
