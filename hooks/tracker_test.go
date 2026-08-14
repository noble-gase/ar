package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// testAgentContext 只覆盖 tracker 会读取的方法，其余 agent.Context 方法由嵌入接口
// 提供，测试不会调用。
type testAgentContext struct {
	agent.Context
	base context.Context

	invocationID string
	agentName    string
	userID       string
	sessionID    string
	branch       string
}

func (c *testAgentContext) Deadline() (time.Time, bool) { return c.base.Deadline() }
func (c *testAgentContext) Done() <-chan struct{}       { return c.base.Done() }
func (c *testAgentContext) Err() error                  { return c.base.Err() }
func (c *testAgentContext) Value(key any) any           { return c.base.Value(key) }
func (c *testAgentContext) InvocationID() string        { return c.invocationID }
func (c *testAgentContext) AgentName() string           { return c.agentName }
func (c *testAgentContext) UserID() string              { return c.userID }
func (c *testAgentContext) SessionID() string           { return c.sessionID }
func (c *testAgentContext) Branch() string              { return c.branch }

func newTrackerContext(invocationID, agentName string) *testAgentContext {
	return &testAgentContext{
		base:         context.Background(),
		invocationID: invocationID,
		agentName:    agentName,
		userID:       "u1",
		sessionID:    "s1",
		branch:       agentName,
	}
}

type testTool string

func (t testTool) Name() string      { return string(t) }
func (testTool) Description() string { return "" }
func (testTool) IsLongRunning() bool { return false }

var _ tool.Tool = testTool("")

func agentByName(t *testing.T, snapshot InvoSnapshot, name string) *AgentTracker {
	t.Helper()
	for _, tracker := range snapshot.Agents {
		if tracker.Name == name {
			return tracker
		}
	}
	t.Fatalf("agent %q not found in snapshot", name)
	return nil
}

// Factory 以 InvocationID 作为生命周期键，两个并存的运行不能互相覆盖。
func TestTrackerSeparatesInvocations(t *testing.T) {
	factory := NewTrackerFactory()

	if _, err := factory.BeforeAgent(newTrackerContext("inv-1", "root")); err != nil {
		t.Fatalf("BeforeAgent(inv-1) error = %v", err)
	}
	if _, err := factory.BeforeAgent(newTrackerContext("inv-2", "root")); err != nil {
		t.Fatalf("BeforeAgent(inv-2) error = %v", err)
	}

	got := factory.Snapshot()
	if len(got) != 2 {
		t.Fatalf("Snapshot() length = %d, want 2", len(got))
	}
	if got[0].InvoID == got[1].InvoID {
		t.Fatalf("invocations collided: %+v", got)
	}
}

// 进行中的快照没有 FinishedAt，序列化时不能出现零值时间。
func TestInProgressSnapshotOmitsFinishedAt(t *testing.T) {
	factory := NewTrackerFactory()
	factory.BeforeAgent(newTrackerContext("inv-1", "root")) //nolint:errcheck

	b, err := json.Marshal(factory.Snapshot()[0])
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(b), "finished_at") {
		t.Errorf("json = %s, want finished_at omitted while in progress", b)
	}
}

func TestTrackerSkipsEmptyInvocationID(t *testing.T) {
	factory := NewTrackerFactory()

	if _, err := factory.BeforeAgent(newTrackerContext("", "root")); err != nil {
		t.Fatalf("BeforeAgent() error = %v", err)
	}
	if got := factory.Snapshot(); len(got) != 0 {
		t.Fatalf("Snapshot() = %+v, want an empty invocation ID skipped", got)
	}
}

// 同名 Agent 在 loop / retry 中重复运行时必须聚合，不能覆盖前一轮的工具和 token。
func TestTrackerAggregatesRepeatedAgentRuns(t *testing.T) {
	factory := NewTrackerFactory()
	var completed InvoSnapshot
	factory.emit = func(_ context.Context, snapshot InvoSnapshot) {
		completed = snapshot
		// emit 必须发生在 Factory 锁外；回调里读取快照不能死锁。
		_ = factory.Snapshot()
	}

	root := newTrackerContext("inv-1", "root")
	worker := newTrackerContext("inv-1", "worker")

	factory.BeforeAgent(root)                                                             //nolint:errcheck
	factory.BeforeAgent(worker)                                                           //nolint:errcheck
	factory.AfterTool(worker, testTool("lookup"), map[string]any{"q": "first"}, nil, nil) //nolint:errcheck
	factory.AfterModel(worker, &model.LLMResponse{
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:        10,
			CandidatesTokenCount:    4,
			ThoughtsTokenCount:      2,
			ToolUsePromptTokenCount: 3,
			CachedContentTokenCount: 5,
			TotalTokenCount:         19,
		},
	}, nil) //nolint:errcheck
	factory.AfterAgent(worker) //nolint:errcheck

	// 第二次运行同名 worker。Partial 分片即便携带 usage 也不能重复计数。
	factory.BeforeAgent(worker) //nolint:errcheck
	factory.AfterModel(worker, &model.LLMResponse{
		Partial: true,
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			TotalTokenCount: 999,
		},
	}, nil) //nolint:errcheck
	factory.AfterModel(worker, &model.LLMResponse{
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     7,
			CandidatesTokenCount: 6,
			ThoughtsTokenCount:   1,
			TotalTokenCount:      14,
		},
	}, nil) //nolint:errcheck
	factory.AfterAgent(worker) //nolint:errcheck
	factory.AfterAgent(root)   //nolint:errcheck

	if completed.InvoID != "inv-1" {
		t.Fatalf("completed invocation = %q, want inv-1", completed.InvoID)
	}
	if len(completed.Agents) != 2 || completed.Agents[0].Name != "root" || completed.Agents[1].Name != "worker" {
		t.Fatalf("agent order = %+v, want stable first-seen order [root worker]", completed.Agents)
	}
	trackedWorker := agentByName(t, completed, "worker")
	if trackedWorker.Runs != 2 {
		t.Errorf("worker runs = %d, want 2", trackedWorker.Runs)
	}
	if len(trackedWorker.Tools) != 1 || trackedWorker.Tools[0].Name != "lookup" {
		t.Errorf("worker tools = %+v, want the first run retained", trackedWorker.Tools)
	}
	want := TokenUsage{
		Prompt:        17,
		Candidates:    10,
		Thoughts:      3,
		ToolUsePrompt: 3,
		Cached:        5,
		Total:         33,
	}
	if trackedWorker.Tokens != want {
		t.Errorf("worker tokens = %+v, want %+v", trackedWorker.Tokens, want)
	}
	if completed.Tokens != want {
		t.Errorf("invocation tokens = %+v, want %+v", completed.Tokens, want)
	}
	if got := factory.Snapshot(); len(got) != 0 {
		t.Fatalf("Snapshot() after completion = %+v, want tracker deleted", got)
	}
}

// 根 Agent 的 AfterAgent 是 invocation 的结束信号。子 Agent 的 AfterAgent 被短路
// 跳过时也必须输出并清理，同时明确标记未完成 activation。
func TestRootCompletionCleansIncompleteChildren(t *testing.T) {
	factory := NewTrackerFactory()
	var completed InvoSnapshot
	factory.emit = func(_ context.Context, snapshot InvoSnapshot) {
		completed = snapshot
	}

	root := newTrackerContext("inv-1", "root")
	child := newTrackerContext("inv-1", "child")
	factory.BeforeAgent(root)  //nolint:errcheck
	factory.BeforeAgent(child) //nolint:errcheck

	// 模拟 child 的 AfterAgent 因 ADK 短路没有发生。
	factory.AfterAgent(root) //nolint:errcheck

	if completed.IncompleteActivations != 1 {
		t.Errorf("incomplete activations = %d, want 1", completed.IncompleteActivations)
	}
	if got := factory.Snapshot(); len(got) != 0 {
		t.Fatalf("Snapshot() = %+v, want incomplete invocation cleaned", got)
	}
}

func TestTrackerTTLAndCapacityBoundMemory(t *testing.T) {
	now := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	factory := NewTrackerFactoryWithConfig(TrackerConfig{
		TTL:            time.Minute,
		MaxInvocations: 2,
	})
	factory.now = func() time.Time { return now }

	factory.BeforeAgent(newTrackerContext("inv-1", "root")) //nolint:errcheck
	now = now.Add(2 * time.Minute)
	factory.BeforeAgent(newTrackerContext("inv-2", "root")) //nolint:errcheck
	if got := factory.Snapshot(); len(got) != 1 || got[0].InvoID != "inv-2" {
		t.Fatalf("Snapshot() after TTL cleanup = %+v, want only inv-2", got)
	}

	now = now.Add(time.Second)
	factory.BeforeAgent(newTrackerContext("inv-3", "root")) //nolint:errcheck
	now = now.Add(time.Second)
	factory.BeforeAgent(newTrackerContext("inv-4", "root")) //nolint:errcheck
	got := factory.Snapshot()
	if len(got) != 2 || got[0].InvoID != "inv-3" || got[1].InvoID != "inv-4" {
		t.Fatalf("Snapshot() at capacity = %+v, want oldest invocation evicted", got)
	}
}

func TestZeroValueTrackerFactoryIsUsable(t *testing.T) {
	var factory TrackerFactory
	ctx := newTrackerContext("inv-1", "root")

	if _, err := factory.BeforeAgent(ctx); err != nil {
		t.Fatalf("BeforeAgent() error = %v", err)
	}
	if got := factory.Snapshot(); len(got) != 1 {
		t.Fatalf("Snapshot() length = %d, want 1", len(got))
	}
	if _, err := factory.AfterAgent(ctx); err != nil {
		t.Fatalf("AfterAgent() error = %v", err)
	}
}

// 并行子 Agent 的 Before/After callback 不得覆盖或触发 data race。
func TestTrackerHandlesParallelAgents(t *testing.T) {
	factory := NewTrackerFactory()
	completed := make(chan InvoSnapshot, 1)
	factory.emit = func(_ context.Context, snapshot InvoSnapshot) {
		completed <- snapshot
	}

	root := newTrackerContext("inv-parallel", "root")
	factory.BeforeAgent(root) //nolint:errcheck

	const children = 32
	contexts := make([]*testAgentContext, children)
	var wg sync.WaitGroup
	wg.Add(children)
	for i := range children {
		contexts[i] = newTrackerContext("inv-parallel", "child-"+string(rune('A'+i)))
		go func(ctx *testAgentContext) {
			defer wg.Done()
			factory.BeforeAgent(ctx) //nolint:errcheck
		}(contexts[i])
	}
	wg.Wait()

	if got := factory.Snapshot(); len(got) != 1 || len(got[0].Agents) != children+1 {
		t.Fatalf("parallel BeforeAgent snapshot = %+v, want %d agents", got, children+1)
	}

	wg.Add(children)
	for _, ctx := range contexts {
		go func(ctx *testAgentContext) {
			defer wg.Done()
			factory.AfterAgent(ctx) //nolint:errcheck
		}(ctx)
	}
	wg.Wait()
	factory.AfterAgent(root) //nolint:errcheck

	select {
	case snapshot := <-completed:
		if len(snapshot.Agents) != children+1 || snapshot.IncompleteActivations != 0 {
			t.Fatalf("completed snapshot = %+v, want all agents complete", snapshot)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tracker did not complete")
	}
}

func TestToolErrorIsRetained(t *testing.T) {
	factory := NewTrackerFactory()
	var completed InvoSnapshot
	factory.emit = func(_ context.Context, snapshot InvoSnapshot) {
		completed = snapshot
	}
	root := newTrackerContext("inv-1", "root")
	wantErr := errors.New("tool failed")

	factory.BeforeAgent(root)                                                          //nolint:errcheck
	factory.AfterTool(root, testTool("danger"), map[string]any{"id": 1}, nil, wantErr) //nolint:errcheck
	factory.AfterAgent(root)                                                           //nolint:errcheck

	if len(completed.Agents) != 1 || len(completed.Agents[0].Tools) != 1 {
		t.Fatalf("tools = %+v, want one call", completed.Agents)
	}
	if !errors.Is(completed.Agents[0].Tools[0].Error, wantErr) {
		t.Errorf("tool error = %v, want %v", completed.Agents[0].Tools[0].Error, wantErr)
	}
}
