package llmchat

import (
	"context"
	"errors"
	"testing"

	argon_session "github.com/noble-gase/argon/session"
	adk_session "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
	"gorm.io/driver/sqlite"
)

func confirmResponseEvent(callId string) *adk_session.Event {
	ev := &adk_session.Event{Author: "user"}
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
	if answeredConfirmations(conversation)["call-1"] {
		t.Fatal("answeredConfirmations() reported an unanswered call as answered")
	}

	// 写入一次确认应答，模拟用户已经做过决定
	if err := sess.Service().AppendEvent(ctx, conversation, confirmResponseEvent("call-1")); err != nil {
		t.Fatalf("AppendEvent() error = %v", err)
	}

	if _, err := chat.Confirm(ctx, "u1", conversation.ID(), "call-1", true, nil); !errors.Is(err, ErrAlreadyConfirmed) {
		t.Fatalf("Confirm() error = %v, want ErrAlreadyConfirmed", err)
	}

	// 另一个 callId 不受影响
	if _, err := chat.Confirm(ctx, "u1", conversation.ID(), "call-2", true, nil); errors.Is(err, ErrAlreadyConfirmed) {
		t.Error("Confirm() rejected an unrelated callId as already answered")
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

func TestAnsweredConfirmationsIgnoresOtherResponses(t *testing.T) {
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

	if answeredConfirmations(conversation)["call-1"] {
		t.Error("answeredConfirmations() counted a human-input reply as a tool confirmation")
	}
}
