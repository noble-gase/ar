package hooks

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

const (
	defaultTrackerTTL     = time.Hour
	defaultMaxInvocations = 10_000
	defaultTrackerSweep   = time.Minute
)

// TrackerConfig 配置 TrackerFactory。零值字段使用默认值。
type TrackerConfig struct {
	// TTL 是异常退出的 invocation 在内存中的最长保留时间。正常完成的记录会立即
	// 输出并删除；TTL 只兜底 AfterAgent 因取消、短路或 panic 没有被调用的情况。
	// 为零时，默认 1 小时。
	TTL time.Duration

	// MaxInvocations 是同时保留的 invocation 上限，避免异常流量或回调缺失导致
	// 无界增长。达到上限时优先淘汰最久未更新的记录。为零时，默认 10,000。
	MaxInvocations int
}

// TrackerFactory 同时服务多个 invocation。内部以 ADK InvocationID 为唯一键；
// trace ID 只用于日志关联，缺失或被调用方复用都不会让不同运行互相覆盖。
type TrackerFactory struct {
	mu sync.RWMutex

	trackers map[string]*invoTracker

	ttl       time.Duration
	maxInvos  int
	lastSweep time.Time

	// 以下字段用于测试注入；生产构造函数会填入默认实现。
	now  func() time.Time
	emit func(context.Context, InvoSnapshot)
}

type trackerContext struct {
	InvoID       string
	UserID       string
	SessionID    string
	AgentName    string
	ActivationID string
}

func trackerContextOf(ctx agent.Context) trackerContext {
	// Path / RunID 只存在于 workflow 节点上下文。Agent / Tool 回调走的是
	// callbackContextWrapper / toolContextWrapper，调用这两个方法只会打
	// "not supported" 日志并返回空串，不能用来区分激活。
	// Branch 在回调上下文可用，配合 AgentName 识别根 Agent vs 嵌套激活。
	return trackerContext{
		InvoID:       ctx.InvocationID(),
		UserID:       ctx.UserID(),
		SessionID:    ctx.SessionID(),
		AgentName:    ctx.AgentName(),
		ActivationID: ctx.AgentName() + "\x00" + ctx.Branch(),
	}
}

func (f *TrackerFactory) timeNow() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

// initLocked 让 TrackerFactory 的零值也可安全使用。
func (f *TrackerFactory) initLocked() {
	if f.trackers == nil {
		f.trackers = make(map[string]*invoTracker)
	}
	if f.ttl <= 0 {
		f.ttl = defaultTrackerTTL
	}
	if f.maxInvos <= 0 {
		f.maxInvos = defaultMaxInvocations
	}
}

// cleanupLocked 低频清理异常退出的记录，并在达到容量上限时淘汰最久未更新的记录。
// 它只由 BeforeAgent 机会式触发，不创建需要 Close 的后台 goroutine。
func (f *TrackerFactory) cleanupLocked(now time.Time) int {
	interval := defaultTrackerSweep
	if half := f.ttl / 2; half > 0 && half < interval {
		interval = half
	}
	if !f.lastSweep.IsZero() && now.Sub(f.lastSweep) < interval && len(f.trackers) < f.maxInvos {
		return 0
	}
	f.lastSweep = now

	removed := 0
	for id, tracker := range f.trackers {
		if now.Sub(tracker.lastUpdated()) >= f.ttl {
			delete(f.trackers, id)
			removed++
		}
	}
	return removed
}

func (f *TrackerFactory) evictOldestLocked() bool {
	var (
		oldestID string
		oldestAt time.Time
	)
	for id, tracker := range f.trackers {
		updatedAt := tracker.lastUpdated()
		if oldestID == "" || updatedAt.Before(oldestAt) {
			oldestID, oldestAt = id, updatedAt
		}
	}
	if oldestID == "" {
		return false
	}
	delete(f.trackers, oldestID)
	return true
}

func (f *TrackerFactory) before(meta trackerContext) int {
	now := f.timeNow()

	f.mu.Lock()
	defer f.mu.Unlock()
	f.initLocked()

	removed := f.cleanupLocked(now)
	tracker := f.trackers[meta.InvoID]
	if tracker == nil {
		for len(f.trackers) >= f.maxInvos {
			if !f.evictOldestLocked() {
				break
			}
			removed++
		}
		tracker = newInvocationTracker(meta, now)
		f.trackers[meta.InvoID] = tracker
	}
	tracker.start(meta, now)
	return removed
}

func (f *TrackerFactory) withTracker(invocationID string, fn func(*invoTracker)) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if tracker := f.trackers[invocationID]; tracker != nil {
		// 持有 Factory 的读锁直到本次更新完成，避免 AfterAgent 在生成最终快照时
		// 同时删除 tracker、漏掉已经开始的 model/tool 回调。
		fn(tracker)
	}
}

func (f *TrackerFactory) after(meta trackerContext, now time.Time) (InvoSnapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	tracker := f.trackers[meta.InvoID]
	if tracker == nil {
		return InvoSnapshot{}, false
	}
	snapshot, complete := tracker.finish(meta, now)
	if complete {
		delete(f.trackers, meta.InvoID)
	}
	return snapshot, complete
}

func (f *TrackerFactory) emitSnapshot(ctx context.Context, snapshot InvoSnapshot) {
	if f.emit != nil {
		f.emit(ctx, snapshot)
		return
	}

	slog.InfoContext(
		ctx, "agent tracker",
		slog.String("invocation_id", snapshot.InvoID),
		slog.String("user_id", snapshot.UserID),
		slog.String("session_id", snapshot.SessionID),
		slog.String("duration", snapshot.FinishedAt.Sub(snapshot.StartedAt).String()),
		slog.Int("incomplete", snapshot.Incomplete),
		slog.GroupAttrs(
			"tokens",
			slog.Int64("prompt", snapshot.Tokens.Prompt),
			slog.Int64("candidates", snapshot.Tokens.Candidates),
			slog.Int64("thoughts", snapshot.Tokens.Thoughts),
			slog.Int64("tool_use_prompt", snapshot.Tokens.ToolUsePrompt),
			slog.Int64("cached", snapshot.Tokens.Cached),
			slog.Int64("total", snapshot.Tokens.Total),
		),
		slog.Any("trace", snapshot.Agents),
	)
}

func (f *TrackerFactory) BeforeAgent(ctx agent.Context) (*genai.Content, error) {
	meta := trackerContextOf(ctx)
	if meta.InvoID == "" {
		// 空 ID 无法安全关联生命周期；跳过比把所有运行塞进同一个空 key 更安全。
		slog.WarnContext(ctx, "[tracker] invocation ID is empty, tracking skipped")
		return nil, nil
	}
	if removed := f.before(meta); removed > 0 {
		slog.WarnContext(ctx, "[tracker] invocation trackers evicted by TTL or capacity", slog.Int("count", removed))
	}
	return nil, nil
}

func (f *TrackerFactory) AfterTool(ctx agent.Context, calledTool tool.Tool, args, result map[string]any, err error) (map[string]any, error) {
	meta := trackerContextOf(ctx)
	f.withTracker(meta.InvoID, func(tracker *invoTracker) {
		tracker.afterTool(meta, calledTool, args, err, f.timeNow())
	})
	return nil, nil
}

func (f *TrackerFactory) AfterModel(ctx agent.Context, resp *model.LLMResponse, err error) (*model.LLMResponse, error) {
	meta := trackerContextOf(ctx)
	f.withTracker(meta.InvoID, func(tracker *invoTracker) {
		tracker.afterModel(meta, resp, f.timeNow())
	})
	return nil, nil
}

func (f *TrackerFactory) AfterAgent(ctx agent.Context) (*genai.Content, error) {
	meta := trackerContextOf(ctx)
	snapshot, complete := f.after(meta, f.timeNow())
	if complete {
		// 日志/回调放在所有锁外，慢 handler 不阻塞其它 tracker callback。
		f.emitSnapshot(ctx, snapshot)
	}
	return nil, nil
}

// Snapshot 返回所有进行中 invocation 的并发安全结构快照，按开始时间、
// InvocationID 稳定排序。调用方无法绕过 Factory 的锁直接读写内部 map。
// ToolCall.Args 仍保留调用方传入的 map 引用，语义与 AfterTool 一致。
func (f *TrackerFactory) Snapshot() []InvoSnapshot {
	now := f.timeNow()

	f.mu.RLock()
	snapshots := make([]InvoSnapshot, 0, len(f.trackers))
	for _, tracker := range f.trackers {
		snapshots = append(snapshots, tracker.snapshot(now, false))
	}
	f.mu.RUnlock()

	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].StartedAt.Equal(snapshots[j].StartedAt) {
			return snapshots[i].InvoID < snapshots[j].InvoID
		}
		return snapshots[i].StartedAt.Before(snapshots[j].StartedAt)
	})
	return snapshots
}

// --------- invocation tracker ---------

type invoTracker struct {
	mu sync.Mutex

	invoID    string
	userID    string
	sessionID string

	startedAt time.Time
	updatedAt time.Time

	rootActivation string
	active         int

	agents map[string]*AgentTracker // key = agent name，重复运行聚合到同一项
	order  []string                 // 首次出现顺序，保证输出稳定
}

func newInvocationTracker(meta trackerContext, now time.Time) *invoTracker {
	return &invoTracker{
		invoID:    meta.InvoID,
		userID:    meta.UserID,
		sessionID: meta.SessionID,
		startedAt: now,
		updatedAt: now,
		agents:    make(map[string]*AgentTracker),
	}
}

func (t *invoTracker) start(meta trackerContext, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.rootActivation == "" {
		t.rootActivation = meta.ActivationID
	}
	tracker := t.agents[meta.AgentName]
	if tracker == nil {
		tracker = &AgentTracker{
			Name: meta.AgentName,
		}
		t.agents[meta.AgentName] = tracker
		t.order = append(t.order, meta.AgentName)
	}
	tracker.Runs++
	tracker.active++
	t.active++
	t.updatedAt = now
}

func (t *invoTracker) afterTool(meta trackerContext, calledTool tool.Tool, args map[string]any, err error, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tracker := t.agents[meta.AgentName]
	if tracker == nil {
		return
	}
	tracker.Tools = append(tracker.Tools, &ToolCall{
		Name:  calledTool.Name(),
		Args:  args,
		Error: err,
	})
	t.updatedAt = now
}

func (t *invoTracker) afterModel(meta trackerContext, resp *model.LLMResponse, now time.Time) {
	// ADK 会为流式分片逐个调用 AfterModel。只统计最终聚合响应，避免 provider
	// 在多个分片附带累计 usage 时重复计数。
	if resp == nil || resp.Partial || resp.UsageMetadata == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	tracker := t.agents[meta.AgentName]
	if tracker == nil {
		return
	}
	tracker.Tokens.add(resp.UsageMetadata)
	t.updatedAt = now
}

func (t *invoTracker) finish(meta trackerContext, now time.Time) (InvoSnapshot, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tracker := t.agents[meta.AgentName]
	if tracker == nil {
		return InvoSnapshot{}, false
	}
	if tracker.active > 0 {
		tracker.active--
		if t.active > 0 {
			t.active--
		}
	}
	t.updatedAt = now

	// 正常嵌套运行在 active == 0 时完成。根 Agent 的 AfterAgent 是 invocation 的
	// 结束信号，即便某个子 Agent 的 AfterAgent 因短路被跳过，也要输出带
	// Incomplete 的快照并清理，避免永久泄漏。
	if t.active != 0 && meta.ActivationID != t.rootActivation {
		return InvoSnapshot{}, false
	}
	return t.snapshotLocked(now, true), true
}

func (t *invoTracker) lastUpdated() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.updatedAt
}

func (t *invoTracker) snapshot(now time.Time, finished bool) InvoSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked(now, finished)
}

func (t *invoTracker) snapshotLocked(now time.Time, finished bool) InvoSnapshot {
	snapshot := InvoSnapshot{
		InvoID:     t.invoID,
		UserID:     t.userID,
		SessionID:  t.sessionID,
		StartedAt:  t.startedAt,
		UpdatedAt:  t.updatedAt,
		Incomplete: t.active,
		Agents:     make([]*AgentTracker, 0, len(t.order)),
	}
	if finished {
		snapshot.FinishedAt = now
	}
	for _, name := range t.order {
		tracker := t.agents[name]
		copied := &AgentTracker{
			Name:   tracker.Name,
			Runs:   tracker.Runs,
			Tools:  append([]*ToolCall(nil), tracker.Tools...),
			Tokens: tracker.Tokens,
		}
		snapshot.Agents = append(snapshot.Agents, copied)
		snapshot.Tokens.addUsage(tracker.Tokens)
	}
	return snapshot
}

// InvoSnapshot 是 invocation 在某个时刻的统计快照。
type InvoSnapshot struct {
	InvoID    string `json:"invocation_id"`
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`

	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// FinishedAt 仅在 invocation 完成后设置；进行中的快照用 omitzero 略去，
	// 避免序列化出零值时间（omitempty 对 time.Time 无效）。
	FinishedAt time.Time `json:"finished_at,omitzero"`

	Incomplete int             `json:"incomplete,omitempty"`
	Agents     []*AgentTracker `json:"agents"`
	Tokens     TokenUsage      `json:"tokens"`
}

// AgentTracker 聚合同一 invocation 内某个 Agent 的所有运行。
type AgentTracker struct {
	Name   string      `json:"name"`
	Runs   int         `json:"runs"`
	Tools  []*ToolCall `json:"tools,omitempty"`
	Tokens TokenUsage  `json:"tokens"`

	active int
}

type ToolCall struct {
	Name  string         `json:"name"`
	Args  map[string]any `json:"args"`
	Error error          `json:"error,omitempty"`
}

// TokenUsage 原样汇总 genai 的各 usage 分项，不用 Total-Prompt 推导含义模糊的
// output。不同 provider 对 thoughts / tool-use 的计费口径不同，保留分项才能让
// 调用方按自己的账单语义处理。
type TokenUsage struct {
	Prompt        int64 `json:"prompt"`
	Candidates    int64 `json:"candidates"`
	Thoughts      int64 `json:"thoughts"`
	ToolUsePrompt int64 `json:"tool_use_prompt"`
	Cached        int64 `json:"cached"`
	Total         int64 `json:"total"`
}

func (u *TokenUsage) add(metadata *genai.GenerateContentResponseUsageMetadata) {
	u.Prompt += int64(metadata.PromptTokenCount)
	u.Candidates += int64(metadata.CandidatesTokenCount)
	u.Thoughts += int64(metadata.ThoughtsTokenCount)
	u.ToolUsePrompt += int64(metadata.ToolUsePromptTokenCount)
	u.Cached += int64(metadata.CachedContentTokenCount)
	u.Total += int64(metadata.TotalTokenCount)
}

func (u *TokenUsage) addUsage(other TokenUsage) {
	u.Prompt += other.Prompt
	u.Candidates += other.Candidates
	u.Thoughts += other.Thoughts
	u.ToolUsePrompt += other.ToolUsePrompt
	u.Cached += other.Cached
	u.Total += other.Total
}

func NewTrackerFactory() *TrackerFactory {
	return NewTrackerFactoryWithConfig(TrackerConfig{})
}

// NewTrackerFactoryWithConfig 返回一个独立的 tracker factory。
func NewTrackerFactoryWithConfig(cfg TrackerConfig) *TrackerFactory {
	if cfg.TTL <= 0 {
		cfg.TTL = defaultTrackerTTL
	}
	if cfg.MaxInvocations <= 0 {
		cfg.MaxInvocations = defaultMaxInvocations
	}
	return &TrackerFactory{
		trackers: make(map[string]*invoTracker),
		ttl:      cfg.TTL,
		maxInvos: cfg.MaxInvocations,
		now:      time.Now,
	}
}

// Tracker 是可直接使用的进程级默认实例。需要独立生命周期或不同容量配置时，
// 使用 NewTrackerFactoryWithConfig 创建自己的实例。
var Tracker = NewTrackerFactory()
