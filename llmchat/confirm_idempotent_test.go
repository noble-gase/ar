package llmchat

import (
	"context"
	"errors"
	"testing"
	"time"

	argon_session "github.com/noble-gase/argon/session"
	adk_session "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
	"gorm.io/driver/sqlite"
)

func confirmResponseEvent(callId string) *adk_session.Event {
	ev := &adk_session.Event{ID: "response-" + callId, Author: "user"}
	ev.Content = &genai.Content{
		Role: string(genai.RoleUser),
		Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				Name:     toolconfirmation.FunctionCallName,
				ID:       callId,
				Response: map[string]any{"confirmed": true},
			},
		}},
	}
	return ev
}

// eventClock 给手工构造的事件发放递增时间戳。会话按 timestamp 排序，时间戳相同的
// 事件顺序不确定；真实事件由 ADK 自动打时间戳。
var eventClock = time.Now()

func nextEventTime() time.Time {
	eventClock = eventClock.Add(time.Second)
	return eventClock
}

func confirmRequestEvent(callId string) *adk_session.Event {
	ev := &adk_session.Event{ID: "request-" + callId, Author: "model", Timestamp: nextEventTime()}
	ev.Content = &genai.Content{
		Role: string(genai.RoleModel),
		Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{
				Name: toolconfirmation.FunctionCallName,
				ID:   callId,
			},
		}},
	}
	return ev
}

// 一个 callId 是否已确认，判定依据必须是会话历史：渠道侧的记录清理可能失败，
// 而带副作用的工具绝不能因为重复点击执行两次。
func TestConfirmRejectsAlreadyAnswered(t *testing.T) {
	sess, err := argon_session.New("idem", sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared&_busy_timeout=5000"))
	if err != nil {
		t.Fatalf("session.New() error = %v", err)
	}

	ctx := context.Background()
	conversation, err := sess.GetOrCreate(ctx, "u1", 0)
	if err != nil {
		t.Fatalf("GetOrCreate() error = %v", err)
	}

	chat := &Chat{session: sess}

	// 尚未确认过：不应被拦截
	if err := sess.Service().AppendEvent(ctx, conversation, confirmRequestEvent("call-1")); err != nil {
		t.Fatalf("AppendEvent(request) error = %v", err)
	}
	if err := resumableConfirmation(conversation, "call-1"); err != nil {
		t.Fatalf("resumableConfirmation() error = %v, want an unanswered request to be resumable", err)
	}

	// 写入一次确认应答，模拟用户已经做过决定
	if err := sess.Service().AppendEvent(ctx, conversation, confirmResponseEvent("call-1")); err != nil {
		t.Fatalf("AppendEvent() error = %v", err)
	}

	if _, err := chat.Confirm(ctx, "u1", conversation.ID(), "call-1", true, nil); !errors.Is(err, ErrAlreadyConfirmed) {
		t.Fatalf("Confirm() error = %v, want ErrAlreadyConfirmed", err)
	}

	// 当前会话里不存在的 callId 不能发给 runner，否则同日重置后的旧卡可能误恢复
	if _, err := chat.Confirm(ctx, "u1", conversation.ID(), "call-2", true, nil); !errors.Is(err, ErrConfirmationNotFound) {
		t.Fatalf("Confirm(unrequested call) error = %v, want ErrConfirmationNotFound", err)
	}
}

// 未决确认必须能从会话历史重建：渠道侧记录丢了，消息路由不能跟着失准。
func TestPendingConfirmationsRebuiltFromSession(t *testing.T) {
	sess, err := argon_session.New("pendingconfirm", sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared&_busy_timeout=5000"))
	if err != nil {
		t.Fatalf("session.New() error = %v", err)
	}

	ctx := context.Background()
	conversation, err := sess.GetOrCreate(ctx, "u1", 0)
	if err != nil {
		t.Fatalf("GetOrCreate() error = %v", err)
	}
	chat := &Chat{session: sess}

	for _, callId := range []string{"call-1", "call-2"} {
		if err := sess.Service().AppendEvent(ctx, conversation, confirmRequestEvent(callId)); err != nil {
			t.Fatalf("AppendEvent(request) error = %v", err)
		}
	}

	got, err := chat.Pending(ctx, "u1")
	if err != nil {
		t.Fatalf("Pending() error = %v", err)
	}
	if len(got.Confirmations) != 2 || got.Confirmations[0].CallId != "call-1" || got.Confirmations[1].CallId != "call-2" {
		t.Fatalf("Pending().Confirmations = %+v, want call-1 and call-2 in order", got.Confirmations)
	}

	// 做过决定的那个不再算未决
	if err := sess.Service().AppendEvent(ctx, conversation, confirmResponseEvent("call-1")); err != nil {
		t.Fatalf("AppendEvent(response) error = %v", err)
	}

	got, err = chat.Pending(ctx, "u1")
	if err != nil {
		t.Fatalf("Pending() error = %v", err)
	}
	if len(got.Confirmations) != 1 || got.Confirmations[0].CallId != "call-2" {
		t.Fatalf("Pending().Confirmations = %+v, want only call-2", got.Confirmations)
	}
}

// 自动会话按自然日轮换。跨日后旧确认指向的执行已被放弃，必须明确报错，而不是
// 悄悄落到当天的新会话上。
func TestConfirmRejectsRotatedConversation(t *testing.T) {
	sess, err := argon_session.New("rotate", sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared&_busy_timeout=5000"))
	if err != nil {
		t.Fatalf("session.New() error = %v", err)
	}

	ctx := context.Background()
	chat := &Chat{session: sess}

	if _, err := chat.Confirm(ctx, "u1", "yesterday", "call-1", true, nil); !errors.Is(err, ErrConversationChanged) {
		t.Fatalf("Confirm() error = %v, want ErrConversationChanged", err)
	}
}

func TestResumableConfirmationIgnoresOtherResponses(t *testing.T) {
	sess, err := argon_session.New("idem2", sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared&_busy_timeout=5000"))
	if err != nil {
		t.Fatalf("session.New() error = %v", err)
	}

	ctx := context.Background()
	conversation, err := sess.GetOrCreate(ctx, "u1", 0)
	if err != nil {
		t.Fatalf("GetOrCreate() error = %v", err)
	}

	// 图工作流的人工输入应答不是工具确认，不能混为一谈
	ev := &adk_session.Event{Author: "user"}
	ev.Content = &genai.Content{
		Role: string(genai.RoleUser),
		Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				Name:     "adk_request_input",
				ID:       "call-1",
				Response: map[string]any{"payload": "hi"},
			},
		}},
	}
	if err := sess.Service().AppendEvent(ctx, conversation, ev); err != nil {
		t.Fatalf("AppendEvent() error = %v", err)
	}

	if err := resumableConfirmation(conversation, "call-1"); !errors.Is(err, ErrConfirmationNotFound) {
		t.Errorf("resumableConfirmation() error = %v, want a human-input reply not to count as a tool confirmation", err)
	}
}
