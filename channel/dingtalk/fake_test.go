package dingtalk

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noble-gase/argon/llmchat"
	"github.com/redis/go-redis/v9"
	"google.golang.org/adk/v2/session"
)

// fakeCard is an in-memory cardStore whose failures can be provoked per method,
// so the Bot's error branches are reachable without Redis or DingTalk.
type fakeCard struct {
	mu    sync.Mutex
	cards []string

	pendings map[string]*pendingConfirm
	leases   map[string]bool
	byUser   map[string][]string
	dropped  []string

	confirmCards int
	answerCards  int
	trackIds     int

	userLocks map[string]*sync.Mutex

	loadErr           error
	saveErr           error
	dropErr           error
	clearConfirmsErr  error
	lockErr           error
	deliverErr        error
	confirmDeliverErr error

	// loseLock 模拟持锁期间被别人接手：返回的 context 立刻取消。
	loseLock bool
}

func newFakeCard() *fakeCard {
	return &fakeCard{
		pendings:  map[string]*pendingConfirm{},
		byUser:    map[string][]string{},
		userLocks: map[string]*sync.Mutex{},
	}
}

func (f *fakeCard) Close() {}

func (f *fakeCard) CreateAndDeliverRobot(context.Context, string) (string, error) {
	if f.deliverErr != nil {
		return "", f.deliverErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answerCards++
	return fmt.Sprintf("answer-%d", f.answerCards), nil
}

func (f *fakeCard) CreateAndDeliverGroup(context.Context, string, string) (string, error) {
	return f.CreateAndDeliverRobot(context.Background(), "")
}

func (f *fakeCard) NewOutTrackId() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trackIds++
	return fmt.Sprintf("confirm-%d", f.trackIds)
}

func (f *fakeCard) DeliverConfirm(_ context.Context, outTrackId string, _ msgMeta, _ string) (string, error) {
	if f.confirmDeliverErr != nil {
		return "", f.confirmDeliverErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirmCards++
	return outTrackId, nil
}

func (f *fakeCard) StreamingUpdate(ctx context.Context, _, content string, _ bool) {
	// 收尾必须用未取消的 context，否则真实实现会调不通钉钉接口
	if ctx.Err() != nil {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.cards = append(f.cards, content)
}

// lastCard returns the most recent card body, or "" when nothing was rendered.
func (f *fakeCard) lastCard() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cards) == 0 {
		return ""
	}
	return f.cards[len(f.cards)-1]
}

func (f *fakeCard) savePending(_ context.Context, outTrackId string, p *pendingConfirm) error {
	if f.saveErr != nil {
		return f.saveErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.pendings[outTrackId] = p
	f.byUser[p.UserId] = append(f.byUser[p.UserId], outTrackId)
	return nil
}

func (f *fakeCard) loadPending(_ context.Context, outTrackId string) (*pendingConfirm, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pendings[outTrackId]
	if !ok {
		return nil, redis.Nil
	}
	return p, nil
}

func (f *fakeCard) dropPending(_ context.Context, outTrackId, userId string) error {
	if f.dropErr != nil {
		return f.dropErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pendings, outTrackId)
	f.dropped = append(f.dropped, outTrackId)

	kept := f.byUser[userId][:0]
	for _, id := range f.byUser[userId] {
		if id != outTrackId {
			kept = append(kept, id)
		}
	}
	f.byUser[userId] = kept
	return nil
}

func (f *fakeCard) clearConfirms(_ context.Context, userId string) error {
	if f.clearConfirmsErr != nil {
		return f.clearConfirmsErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.byUser[userId] {
		delete(f.pendings, id)
	}
	delete(f.byUser, userId)
	return nil
}

// pendingCount reports how many confirmations are still awaiting a decision.
func (f *fakeCard) pendingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pendings)
}

// onlyPending returns the single stored confirmation, failing when there is not
// exactly one.
func (f *fakeCard) onlyPending(t *testing.T) *pendingConfirm {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pendings) != 1 {
		t.Fatalf("pendingCount = %d, want exactly 1", len(f.pendings))
	}
	for _, p := range f.pendings {
		return p
	}
	return nil
}

// deliveredConfirms counts the confirmation cards that were sent.
func (f *fakeCard) deliveredConfirms() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.confirmCards
}

func (f *fakeCard) lockUser(ctx context.Context, userId string) (context.Context, func(), error) {
	if f.lockErr != nil {
		return nil, nil, f.lockErr
	}

	f.mu.Lock()
	m, ok := f.userLocks[userId]
	if !ok {
		m = &sync.Mutex{}
		f.userLocks[userId] = m
	}
	f.mu.Unlock()

	m.Lock()

	held, lost := context.WithCancel(ctx)
	if f.loseLock {
		lost()
	}
	return held, func() {
		lost()
		m.Unlock()
	}, nil
}

// fakeChat records what the Bot sent and replays a scripted event stream. Its
// pending list stands in for the ADK session, the single source of truth.
type fakeChat struct {
	mu sync.Mutex

	events    []*session.Event
	replyErr  error
	streamErr error

	pending    []*llmchat.RequestInput
	pendingErr error

	askedText  string
	repliedId  string
	repliedArg any
	resetCalls int
	resetErr   error

	// confirmed receives every Confirm call; resumeConfirmed runs in its own
	// goroutine, so tests wait on it instead of sleeping.
	confirmed chan confirmCall

	// panicOnConfirm 模拟恢复过程中的 panic。
	panicOnConfirm bool

	// confirmErr 让 Confirm 直接返回指定错误，用于模拟「已确认过」。
	confirmErr error

	// sessionId 是 Ask/Reply 声称的运行会话。留空表示不校验，非空时 Confirm 会
	// 像真实实现那样拒绝指向其它会话的确认。
	sessionId string
}

type confirmCall struct {
	callId   string
	approved bool
}

func (f *fakeChat) stream(events []*session.Event) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
		if f.streamErr != nil {
			yield(nil, f.streamErr)
		}
	}
}

func (f *fakeChat) Ask(_ context.Context, _, text string) (string, iter.Seq2[*session.Event, error], error) {
	f.mu.Lock()
	f.askedText = text
	f.mu.Unlock()
	return f.sessionId, f.stream(f.events), nil
}

func (f *fakeChat) Reply(_ context.Context, _, interruptId string, payload any) (string, iter.Seq2[*session.Event, error], error) {
	f.mu.Lock()
	f.repliedId = interruptId
	f.repliedArg = payload
	// 回答被接受即视为该请求已解决，与会话的行为一致；失败则原样留在待答列表里
	if f.replyErr == nil && f.streamErr == nil && len(f.pending) != 0 {
		f.pending = f.pending[1:]
	}
	replyErr := f.replyErr
	f.mu.Unlock()

	if replyErr != nil {
		return "", nil, replyErr
	}
	return f.sessionId, f.stream(f.events), nil
}

func (f *fakeChat) Confirm(_ context.Context, _, conversationId, callId string, approved bool, _ any) (iter.Seq2[*session.Event, error], error) {
	if f.sessionId != "" && conversationId != f.sessionId {
		return nil, llmchat.ErrConversationChanged
	}
	if f.confirmed != nil {
		f.confirmed <- confirmCall{callId: callId, approved: approved}
	}
	if f.panicOnConfirm {
		panic("boom")
	}
	if f.confirmErr != nil {
		return nil, f.confirmErr
	}
	return f.stream(f.events), nil
}

func (f *fakeChat) PendingInputs(context.Context, string) ([]*llmchat.RequestInput, error) {
	if f.pendingErr != nil {
		return nil, f.pendingErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*llmchat.RequestInput(nil), f.pending...), nil
}

func (f *fakeChat) ResetAutomatic(context.Context, string) error {
	f.mu.Lock()
	f.resetCalls++
	if f.resetErr == nil {
		f.pending = nil
	}
	resetErr := f.resetErr
	f.mu.Unlock()
	return resetErr
}

var errRedisDown = errors.New("redis down")

// concurrencyChat measures how many turns overlap, to prove per-user
// serialization.
type concurrencyChat struct {
	inFlight      atomic.Int32
	maxConcurrent atomic.Int32
	calls         atomic.Int32

	// hold, when non-nil, blocks every turn until it is closed.
	hold chan struct{}
}

func (c *concurrencyChat) enter() {
	c.calls.Add(1)
	now := c.inFlight.Add(1)
	for {
		peak := c.maxConcurrent.Load()
		if now <= peak || c.maxConcurrent.CompareAndSwap(peak, now) {
			break
		}
	}

	if c.hold != nil {
		<-c.hold
	} else {
		time.Sleep(time.Millisecond)
	}

	c.inFlight.Add(-1)
}

func (c *concurrencyChat) Ask(context.Context, string, string) (string, iter.Seq2[*session.Event, error], error) {
	c.enter()
	return "", func(func(*session.Event, error) bool) {}, nil
}

func (c *concurrencyChat) Reply(context.Context, string, string, any) (string, iter.Seq2[*session.Event, error], error) {
	c.enter()
	return "", func(func(*session.Event, error) bool) {}, nil
}

func (c *concurrencyChat) Confirm(context.Context, string, string, string, bool, any) (iter.Seq2[*session.Event, error], error) {
	c.enter()
	return func(func(*session.Event, error) bool) {}, nil
}

func (c *concurrencyChat) PendingInputs(context.Context, string) ([]*llmchat.RequestInput, error) {
	return nil, nil
}

func (c *concurrencyChat) ResetAutomatic(context.Context, string) error { return nil }

func newTestBot(card cardStore, chat chatClient) *Bot {
	return &Bot{
		chat:           chat,
		card:           card,
		timeout:        defaultRequestTimeout,
		cancelKeywords: defaultCancelKeywords,
	}
}
