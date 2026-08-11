package anthropic

import (
	"reflect"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
)

func TestRepairMessageHistoryFiltersBothSides(t *testing.T) {
	messages := []sdk.MessageParam{
		{
			Role: sdk.MessageParamRoleAssistant,
			Content: []sdk.ContentBlockParamUnion{
				sdk.NewTextBlock("before"),
				sdk.NewToolUseBlock("matched", map[string]any{}, "matched_tool"),
				sdk.NewToolUseBlock("orphan_use", map[string]any{}, "orphan_tool"),
			},
		},
		{
			Role: sdk.MessageParamRoleUser,
			Content: []sdk.ContentBlockParamUnion{
				sdk.NewToolResultBlock("matched", `{"ok":true}`, false),
				sdk.NewToolResultBlock("orphan_result", `{"ok":false}`, false),
				sdk.NewTextBlock("after"),
			},
		},
	}

	got := repairMessageHistory(messages)
	if len(got) != 2 {
		t.Fatalf("repairMessageHistory() returned %d messages, want 2", len(got))
	}
	if ids := extractToolUseIds(got[0]); !reflect.DeepEqual(ids, []string{"matched"}) {
		t.Fatalf("tool_use IDs = %v, want [matched]", ids)
	}
	if ids := extractToolResultIds(got[1]); !reflect.DeepEqual(ids, []string{"matched"}) {
		t.Fatalf("tool_result IDs = %v, want [matched]", ids)
	}
	if got[0].Content[0].OfText == nil || got[0].Content[0].OfText.Text != "before" {
		t.Fatal("assistant text block was not preserved")
	}
	if got[1].Content[len(got[1].Content)-1].OfText == nil || got[1].Content[len(got[1].Content)-1].OfText.Text != "after" {
		t.Fatal("user text block was not preserved")
	}
}

func TestRepairMessageHistoryRemovesStandaloneToolResult(t *testing.T) {
	messages := []sdk.MessageParam{{
		Role: sdk.MessageParamRoleUser,
		Content: []sdk.ContentBlockParamUnion{
			sdk.NewToolResultBlock("orphan", `{"ok":false}`, false),
			sdk.NewTextBlock("keep me"),
		},
	}}

	got := repairMessageHistory(messages)
	if len(got) != 1 {
		t.Fatalf("repairMessageHistory() returned %d messages, want 1", len(got))
	}
	if ids := extractToolResultIds(got[0]); len(ids) != 0 {
		t.Fatalf("tool_result IDs = %v, want none", ids)
	}
	if len(got[0].Content) != 1 || got[0].Content[0].OfText == nil || got[0].Content[0].OfText.Text != "keep me" {
		t.Fatal("non-tool content was not preserved")
	}
}

func TestRepairMessageHistoryKeepsMatchedPair(t *testing.T) {
	messages := []sdk.MessageParam{
		{
			Role:    sdk.MessageParamRoleAssistant,
			Content: []sdk.ContentBlockParamUnion{sdk.NewToolUseBlock("call", map[string]any{"q": "value"}, "tool")},
		},
		{
			Role:    sdk.MessageParamRoleUser,
			Content: []sdk.ContentBlockParamUnion{sdk.NewToolResultBlock("call", `{"ok":true}`, false)},
		},
	}

	got := repairMessageHistory(messages)
	if len(got) != 2 {
		t.Fatalf("repairMessageHistory() returned %d messages, want 2", len(got))
	}
	if ids := extractToolUseIds(got[0]); !reflect.DeepEqual(ids, []string{"call"}) {
		t.Fatalf("tool_use IDs = %v, want [call]", ids)
	}
	if ids := extractToolResultIds(got[1]); !reflect.DeepEqual(ids, []string{"call"}) {
		t.Fatalf("tool_result IDs = %v, want [call]", ids)
	}
}

func TestRepairMessageHistoryOrphanAcrossConsecutiveAssistants(t *testing.T) {
	messages := []sdk.MessageParam{
		{
			Role:    sdk.MessageParamRoleAssistant,
			Content: []sdk.ContentBlockParamUnion{sdk.NewToolUseBlock("A", map[string]any{}, "tool")},
		},
		{
			Role:    sdk.MessageParamRoleAssistant,
			Content: []sdk.ContentBlockParamUnion{sdk.NewToolUseBlock("B", map[string]any{}, "tool")},
		},
		{
			Role:    sdk.MessageParamRoleUser,
			Content: []sdk.ContentBlockParamUnion{sdk.NewToolResultBlock("B", `{"ok":true}`, false)},
		},
	}

	// A 没有配对的 tool_result，它那条（已变空的）assistant 消息会被丢弃；
	// B 与结果配对，因此保留。
	got := repairMessageHistory(messages)
	if len(got) != 2 {
		t.Fatalf("repairMessageHistory() returned %d messages, want 2", len(got))
	}
	if ids := extractToolUseIds(got[0]); !reflect.DeepEqual(ids, []string{"B"}) {
		t.Fatalf("tool_use IDs = %v, want [B]", ids)
	}
	if ids := extractToolResultIds(got[1]); !reflect.DeepEqual(ids, []string{"B"}) {
		t.Fatalf("tool_result IDs = %v, want [B]", ids)
	}
}
