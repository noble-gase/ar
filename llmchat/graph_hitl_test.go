package llmchat

import (
	"context"
	"iter"
	"sync/atomic"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	argon_session "github.com/noble-gase/argon/session"
	"google.golang.org/adk/v2/agent"
	adk_session "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"gorm.io/driver/sqlite"
)

// TestGraphAgentHumanInputRoundTrip 端到端验证图工作流的暂停与恢复：节点发出
// RequestInput 后整轮结束，用户的回答经 Reply 送回后必须落到后继节点的输入上，
// 且暂停节点不会被重跑。走真实的 runner 与数据库会话，不打桩。
func TestGraphAgentHumanInputRoundTrip(t *testing.T) {
	var (
		askRuns  atomic.Int32
		greetGot atomic.Value
	)

	ga := &GraphAgent{
		Name: "hitl",
		Edges: func(g *Graph) []workflow.Edge {
			ask := workflow.NewEmittingFunctionNode(
				"ask",
				func(ctx agent.Context, _ any, emit func(*adk_session.Event) error) (any, error) {
					askRuns.Add(1)
					if err := emit(workflow.NewRequestInputEvent(ctx, adk_session.RequestInput{
						InterruptID: "ask-1",
						Message:     "你的名字？",
					})); err != nil {
						return nil, err
					}
					return nil, workflow.ErrNodeInterrupted
				},
				workflow.NodeConfig{},
			)
			greet := workflow.NewFunctionNode(
				"greet",
				func(_ agent.Context, name string) (string, error) {
					greetGot.Store(name)
					return "hello " + name, nil
				},
				workflow.NodeConfig{},
			)
			return workflow.Chain(workflow.Start, ask, greet)
		},
	}

	root, err := ga.Build(nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	sess, err := argon_session.New("hitl", sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared&_busy_timeout=5000"))
	if err != nil {
		t.Fatalf("session.New() error = %v", err)
	}

	chat, err := NewChat(root, sess)
	if err != nil {
		t.Fatalf("NewChat() error = %v", err)
	}

	ctx := context.Background()

	// 第一轮：图应在 ask 节点暂停并抛出人工输入请求
	_, seq, err := chat.Ask(ctx, "u1", "开始")
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}

	var interruptId string
	for event, err := range seq {
		if err != nil {
			t.Fatalf("Ask() event error = %v", err)
		}
		if in, ok := RequestInputOf(event); ok {
			interruptId = in.InterruptId
		}
	}
	if interruptId == "" {
		t.Fatal("first turn did not surface a human-input request")
	}
	if greetGot.Load() != nil {
		t.Fatalf("greet ran during the paused turn, got %v", greetGot.Load())
	}

	// 第二轮：回答必须送达 greet
	_, resumed, err := chat.Reply(ctx, "u1", interruptId, "Alice")
	if err != nil {
		t.Fatalf("Reply() error = %v", err)
	}
	for _, err := range resumed {
		if err != nil {
			t.Fatalf("Reply() event error = %v", err)
		}
	}

	if got := greetGot.Load(); got != "Alice" {
		t.Errorf("greet input = %v, want %q", got, "Alice")
	}
	if n := askRuns.Load(); n != 1 {
		t.Errorf("ask ran %d times, want 1 (resume must not re-run the paused node)", n)
	}
}

// TestGraphAgentConcurrentHumanInput 验证扇出导致多个节点在同一轮同时暂停时，
// 一轮里能拿到全部请求，且逐个回答能各自恢复对应分支。钉钉侧的待回答队列依赖
// 这个行为。
func TestGraphAgentConcurrentHumanInput(t *testing.T) {
	var gotA, gotB atomic.Value

	askNode := func(name, interruptId string) workflow.Node {
		return workflow.NewEmittingFunctionNode(
			name,
			func(ctx agent.Context, _ any, emit func(*adk_session.Event) error) (any, error) {
				if err := emit(workflow.NewRequestInputEvent(ctx, adk_session.RequestInput{
					InterruptID: interruptId,
					Message:     name + " ?",
				})); err != nil {
					return nil, err
				}
				return nil, workflow.ErrNodeInterrupted
			},
			workflow.NodeConfig{},
		)
	}
	sinkNode := func(name string, dst *atomic.Value) workflow.Node {
		return workflow.NewFunctionNode(name, func(_ agent.Context, in string) (string, error) {
			dst.Store(in)
			return in, nil
		}, workflow.NodeConfig{})
	}

	ga := &GraphAgent{
		Name: "hitl_fanout",
		Edges: func(g *Graph) []workflow.Edge {
			askA, askB := askNode("ask_a", "ask-a"), askNode("ask_b", "ask-b")

			eb := workflow.NewEdgeBuilder()
			eb.AddFanOut(workflow.Start, askA, askB)
			eb.Add(askA, sinkNode("sink_a", &gotA))
			eb.Add(askB, sinkNode("sink_b", &gotB))
			return eb.Build()
		},
	}

	root, err := ga.Build(nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	sess, err := argon_session.New("hitl_fanout", sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared&_busy_timeout=5000"))
	if err != nil {
		t.Fatalf("session.New() error = %v", err)
	}
	chat, err := NewChat(root, sess)
	if err != nil {
		t.Fatalf("NewChat() error = %v", err)
	}

	ctx := context.Background()

	_, seq, err := chat.Ask(ctx, "u1", "开始")
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}

	var ids []string
	for event, err := range seq {
		if err != nil {
			t.Fatalf("Ask() event error = %v", err)
		}
		if in, ok := RequestInputOf(event); ok {
			ids = append(ids, in.InterruptId)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("first turn surfaced %d human-input requests (%v), want 2", len(ids), ids)
	}

	// 逐个回答，每次只解开一个分支
	answers := map[string]string{"ask-a": "A", "ask-b": "B"}
	for _, id := range ids {
		_, resumed, err := chat.Reply(ctx, "u1", id, answers[id])
		if err != nil {
			t.Fatalf("Reply(%s) error = %v", id, err)
		}
		for _, err := range resumed {
			if err != nil {
				t.Fatalf("Reply(%s) event error = %v", id, err)
			}
		}
	}

	if got := gotA.Load(); got != "A" {
		t.Errorf("sink_a input = %v, want %q", got, "A")
	}
	if got := gotB.Load(); got != "B" {
		t.Errorf("sink_b input = %v, want %q", got, "B")
	}
}

// TestGraphAgentRejectedReplyCanBeRetried 验证节点声明 ResponseSchema 时：不合规
// 的回答会被识别为 IsRejectedReply 且节点保持 waiting，用户可以重答成功。钉钉侧
// 「答案被拒就把问题放回队头」依赖这个行为。
func TestGraphAgentRejectedReplyCanBeRetried(t *testing.T) {
	var sinkGot atomic.Value

	schema := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"approved": {Type: "boolean"},
		},
	}

	ga := &GraphAgent{
		Name: "hitl_schema",
		Edges: func(g *Graph) []workflow.Edge {
			ask := workflow.NewEmittingFunctionNode(
				"ask",
				func(ctx agent.Context, _ any, emit func(*adk_session.Event) error) (any, error) {
					if err := emit(workflow.NewRequestInputEvent(ctx, adk_session.RequestInput{
						InterruptID:    "ask-schema",
						Message:        "是否批准？",
						ResponseSchema: schema,
					})); err != nil {
						return nil, err
					}
					return nil, workflow.ErrNodeInterrupted
				},
				workflow.NodeConfig{},
			)
			sink := workflow.NewFunctionNode(
				"sink",
				func(_ agent.Context, in map[string]any) (any, error) {
					sinkGot.Store(in["approved"])
					return in, nil
				},
				workflow.NodeConfig{},
			)
			return workflow.Chain(workflow.Start, ask, sink)
		},
	}

	root, err := ga.Build(nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	sess, err := argon_session.New("hitl_schema", sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared&_busy_timeout=5000"))
	if err != nil {
		t.Fatalf("session.New() error = %v", err)
	}
	chat, err := NewChat(root, sess)
	if err != nil {
		t.Fatalf("NewChat() error = %v", err)
	}

	ctx := context.Background()

	_, seq, err := chat.Ask(ctx, "u1", "开始")
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}

	var got *RequestInput
	for event, err := range seq {
		if err != nil {
			t.Fatalf("Ask() event error = %v", err)
		}
		if in, ok := RequestInputOf(event); ok {
			got = in
		}
	}
	if got == nil {
		t.Fatal("first turn did not surface a human-input request")
	}
	if got.ResponseSchema == nil {
		t.Fatal("RequestInputOf() lost ResponseSchema, the channel cannot tell the user what shape to answer in")
	}

	// 第一次用纯文本回答：不符合 schema，必须被识别为可重试
	rejected := chat.mustReply(ctx, t, "u1", got.InterruptId, ReplyPayload("同意", got.ResponseSchema))
	if !IsRejectedReply(rejected) {
		t.Fatalf("free-form answer error = %v, want IsRejectedReply", rejected)
	}
	if sinkGot.Load() != nil {
		t.Fatalf("sink ran on a rejected answer, got %v", sinkGot.Load())
	}

	// 重答一个合规的 JSON：节点仍在 waiting，应当恢复成功
	if err := chat.mustReply(ctx, t, "u1", got.InterruptId, ReplyPayload(`{"approved":true}`, got.ResponseSchema)); err != nil {
		t.Fatalf("retry error = %v, want nil", err)
	}
	if sinkGot.Load() != true {
		t.Errorf("sink approved = %v, want true", sinkGot.Load())
	}
}

// TestGraphAgentResumeDoesNotReEmitPendingRequests 锁定一个关键前提：恢复其中
// 一个分支时，仍在 waiting 的其他分支不会重新发出 RequestInput。因此渠道层保存
// 待答队列必须是追加语义——一旦覆盖，未答分支的 interruptId 就永久丢失了。
func TestGraphAgentResumeDoesNotReEmitPendingRequests(t *testing.T) {
	askNode := func(name, interruptId string) workflow.Node {
		return workflow.NewEmittingFunctionNode(
			name,
			func(ctx agent.Context, _ any, emit func(*adk_session.Event) error) (any, error) {
				if err := emit(workflow.NewRequestInputEvent(ctx, adk_session.RequestInput{
					InterruptID: interruptId,
					Message:     name + " ?",
				})); err != nil {
					return nil, err
				}
				return nil, workflow.ErrNodeInterrupted
			},
			workflow.NodeConfig{},
		)
	}

	ga := &GraphAgent{
		Name: "hitl_chained_fanout",
		Edges: func(g *Graph) []workflow.Edge {
			askA, askB := askNode("ask_a", "ask-a"), askNode("ask_b", "ask-b")
			askA2 := askNode("ask_a2", "ask-a2")

			eb := workflow.NewEdgeBuilder()
			eb.AddFanOut(workflow.Start, askA, askB)
			eb.Add(askA, askA2)
			return eb.Build()
		},
	}

	root, err := ga.Build(nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	sess, err := argon_session.New("chained", sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared&_busy_timeout=5000"))
	if err != nil {
		t.Fatalf("session.New() error = %v", err)
	}
	chat, err := NewChat(root, sess)
	if err != nil {
		t.Fatalf("NewChat() error = %v", err)
	}

	ctx := context.Background()

	collect := func(seq iter.Seq2[*adk_session.Event, error]) []string {
		t.Helper()
		var ids []string
		for event, err := range seq {
			if err != nil {
				t.Fatalf("event error = %v", err)
			}
			if in, ok := RequestInputOf(event); ok {
				ids = append(ids, in.InterruptId)
			}
		}
		return ids
	}

	_, seq, err := chat.Ask(ctx, "u1", "开始")
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if got := collect(seq); len(got) != 2 {
		t.Fatalf("first turn requests = %v, want 2", got)
	}

	// 只回答 a 分支：新暂停点 ask-a2 出现，但仍在 waiting 的 ask-b 不会重发
	_, resumed, err := chat.Reply(ctx, "u1", "ask-a", "A")
	if err != nil {
		t.Fatalf("Reply() error = %v", err)
	}
	got := collect(resumed)

	if len(got) != 1 || got[0] != "ask-a2" {
		t.Fatalf("resume turn requests = %v, want only [ask-a2]", got)
	}
	for _, id := range got {
		if id == "ask-b" {
			t.Fatal("ask-b was re-emitted; the channel could rely on overwrite semantics")
		}
	}
}

// TestPendingInputsRebuiltFromSession 验证待答问题可以完全从会话历史重建：渠道
// 侧的缓存会过期或故障，session 才是唯一真相来源。
func TestPendingInputsRebuiltFromSession(t *testing.T) {
	askNode := func(name, interruptId string) workflow.Node {
		return workflow.NewEmittingFunctionNode[any, any](
			name,
			func(ctx agent.Context, _ any, emit func(*adk_session.Event) error) (any, error) {
				if err := emit(workflow.NewRequestInputEvent(ctx, adk_session.RequestInput{
					InterruptID: interruptId,
					Message:     name + " ?",
				})); err != nil {
					return nil, err
				}
				return nil, workflow.ErrNodeInterrupted
			},
			workflow.NodeConfig{},
		)
	}

	ga := &GraphAgent{
		Name: "pending_rebuild",
		Edges: func(g *Graph) []workflow.Edge {
			askA, askB := askNode("ask_a", "ask-a"), askNode("ask_b", "ask-b")
			sink := workflow.NewJoinNode("gather")

			eb := workflow.NewEdgeBuilder()
			eb.AddFanOut(workflow.Start, askA, askB)
			eb.AddFanIn(sink, askA, askB)
			return eb.Build()
		},
	}

	root, err := ga.Build(nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	sess, err := argon_session.New("pending", sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared&_busy_timeout=5000"))
	if err != nil {
		t.Fatalf("session.New() error = %v", err)
	}
	chat, err := NewChat(root, sess)
	if err != nil {
		t.Fatalf("NewChat() error = %v", err)
	}

	ctx := context.Background()

	_, seq, err := chat.Ask(ctx, "u1", "开始")
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	for _, err := range seq {
		if err != nil {
			t.Fatalf("Ask() event error = %v", err)
		}
	}

	pending, err := chat.PendingInputs(ctx, "u1")
	if err != nil {
		t.Fatalf("PendingInputs() error = %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("PendingInputs() = %d requests, want 2", len(pending))
	}
	if pending[0].Message == "" {
		t.Error("PendingInputs() lost the prompt text, the channel could not re-ask")
	}

	// 回答其中一个后，它必须从待答列表里消失，另一个仍在
	_, resumed, err := chat.Reply(ctx, "u1", pending[0].InterruptId, "A")
	if err != nil {
		t.Fatalf("Reply() error = %v", err)
	}
	for _, err := range resumed {
		if err != nil {
			t.Fatalf("Reply() event error = %v", err)
		}
	}

	after, err := chat.PendingInputs(ctx, "u1")
	if err != nil {
		t.Fatalf("PendingInputs() after reply error = %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("PendingInputs() after reply = %d, want 1", len(after))
	}
	if after[0].InterruptId == pending[0].InterruptId {
		t.Errorf("answered request %q is still listed as pending", after[0].InterruptId)
	}

	// 全部答完后列表为空
	_, final, err := chat.Reply(ctx, "u1", after[0].InterruptId, "B")
	if err != nil {
		t.Fatalf("Reply() error = %v", err)
	}
	for _, err := range final {
		if err != nil {
			t.Fatalf("Reply() event error = %v", err)
		}
	}
	if got, err := chat.PendingInputs(ctx, "u1"); err != nil || len(got) != 0 {
		t.Errorf("PendingInputs() = %v (err=%v), want empty", got, err)
	}
}

// TestPendingInputsKeepsRejectedAnswer 验证不合 schema 的回答不会让问题从待答
// 列表里消失：ADK 会拒绝它、节点仍然 parked，渠道必须能据此再问一次。
func TestPendingInputsKeepsRejectedAnswer(t *testing.T) {
	schema := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"approved": {Type: "boolean"},
		},
	}

	var sinkGot atomic.Value

	ga := &GraphAgent{
		Name: "reject_pending",
		Edges: func(g *Graph) []workflow.Edge {
			askNode := workflow.NewEmittingFunctionNode[any, any](
				"ask",
				func(ctx agent.Context, _ any, emit func(*adk_session.Event) error) (any, error) {
					if err := emit(workflow.NewRequestInputEvent(ctx, adk_session.RequestInput{
						InterruptID:    "ask-schema",
						Message:        "是否批准？",
						ResponseSchema: schema,
					})); err != nil {
						return nil, err
					}
					return nil, workflow.ErrNodeInterrupted
				},
				workflow.NodeConfig{},
			)
			sink := workflow.NewFunctionNode(
				"sink",
				func(_ agent.Context, in map[string]any) (any, error) {
					sinkGot.Store(in["approved"])
					return in, nil
				},
				workflow.NodeConfig{},
			)
			return workflow.Chain(workflow.Start, askNode, sink)
		},
	}

	root, err := ga.Build(nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	sess, err := argon_session.New("reject", sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared&_busy_timeout=5000"))
	if err != nil {
		t.Fatalf("session.New() error = %v", err)
	}
	chat, err := NewChat(root, sess)
	if err != nil {
		t.Fatalf("NewChat() error = %v", err)
	}

	ctx := context.Background()

	_, seq, err := chat.Ask(ctx, "u1", "开始")
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	for _, err := range seq {
		if err != nil {
			t.Fatalf("Ask() event error = %v", err)
		}
	}

	pending, err := chat.PendingInputs(ctx, "u1")
	if err != nil || len(pending) != 1 {
		t.Fatalf("PendingInputs() = %v (err=%v), want 1 request", pending, err)
	}

	// 用纯文本回答，schema 要求对象，ADK 会拒绝
	if err := chat.mustReply(ctx, t, "u1", pending[0].InterruptId, "同意"); !IsRejectedReply(err) {
		t.Fatalf("reply error = %v, want IsRejectedReply", err)
	}

	// 关键：问题必须仍在待答列表里，否则用户再也无法重答
	stillPending, err := chat.PendingInputs(ctx, "u1")
	if err != nil {
		t.Fatalf("PendingInputs() error = %v", err)
	}
	if len(stillPending) != 1 || stillPending[0].InterruptId != pending[0].InterruptId {
		t.Fatalf("PendingInputs() after a rejected answer = %v, want the question still pending", stillPending)
	}

	// 重答合规内容后才算解决
	if err := chat.mustReply(ctx, t, "u1", stillPending[0].InterruptId, map[string]any{"approved": true}); err != nil {
		t.Fatalf("retry error = %v", err)
	}
	if sinkGot.Load() != true {
		t.Errorf("sink approved = %v, want true", sinkGot.Load())
	}
	if got, err := chat.PendingInputs(ctx, "u1"); err != nil || len(got) != 0 {
		t.Errorf("PendingInputs() = %v (err=%v), want empty once answered", got, err)
	}
}

// mustReply 发起一次回答并返回事件流里的第一个错误。
func (c *Chat) mustReply(ctx context.Context, t *testing.T, userId, interruptId string, payload any) error {
	t.Helper()

	_, seq, err := c.Reply(ctx, userId, interruptId, payload)
	if err != nil {
		return err
	}
	for _, err := range seq {
		if err != nil {
			return err
		}
	}
	return nil
}
