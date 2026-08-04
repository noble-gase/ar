package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"
	"gorm.io/driver/sqlite"
)

func newTestManager(t *testing.T) *Session {
	t.Helper()

	repo, err := NewConversationRepository(sqlite.Open("file:" + t.Name() + "?mode=memory&cache=shared&_busy_timeout=5000"))
	if err != nil {
		t.Fatalf("create conversation repository: %v", err)
	}
	return &Session{name: "test", service: adksession.InMemoryService(), convRepo: repo}
}

func TestExplicitConversations(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t)

	created, err := manager.CreateConversation(ctx, "user-1", "conversation-1")
	if err != nil {
		t.Fatalf("CreateConversation() error = %v", err)
	}
	if created.ID() != "conversation-1" {
		t.Fatalf("CreateConversation() ID = %q, want %q", created.ID(), "conversation-1")
	}

	got, err := manager.GetConversation(ctx, "user-1", "conversation-1")
	if err != nil {
		t.Fatalf("GetConversation() error = %v", err)
	}
	if got.UserID() != "user-1" {
		t.Fatalf("GetConversation() UserID = %q, want %q", got.UserID(), "user-1")
	}

	page, err := manager.ListConversations(ctx, "user-1", "", 20)
	if err != nil {
		t.Fatalf("ListConversations() error = %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("ListConversations() length = %d, want 1", len(page.Items))
	}

	if _, err := manager.GetConversation(ctx, "user-2", "conversation-1"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("GetConversation() wrong owner error = %v, want ErrConversationNotFound", err)
	}

	if err := manager.DeleteConversation(ctx, "user-1", "conversation-1"); err != nil {
		t.Fatalf("DeleteConversation() error = %v", err)
	}
	if _, err := manager.GetConversation(ctx, "user-1", "conversation-1"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("GetConversation() after delete error = %v, want ErrConversationNotFound", err)
	}
}

func TestCreateConversationGeneratesID(t *testing.T) {
	manager := newTestManager(t)

	created, err := manager.CreateConversation(context.Background(), "user-1", "")
	if err != nil {
		t.Fatalf("CreateConversation() error = %v", err)
	}
	if created.ID() == "" {
		t.Fatal("CreateConversation() generated an empty ID")
	}
}

func TestConversationModesAreIsolated(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t)
	service := manager.service

	explicit, err := manager.CreateConversation(ctx, "user-1", "explicit-1")
	if err != nil {
		t.Fatalf("CreateConversation() error = %v", err)
	}
	if !hasSessionMode(explicit, sessionModeExplicit) {
		t.Fatal("CreateConversation() did not mark the session explicit")
	}

	_, err = service.Create(ctx, &adksession.CreateRequest{
		AppName:   "test",
		UserID:    "user-1",
		SessionID: "auto-1",
		State: map[string]any{
			sessionModeKey: sessionModeAuto,
		},
	})
	if err != nil {
		t.Fatalf("create auto session error = %v", err)
	}
	_, err = service.Create(ctx, &adksession.CreateRequest{
		AppName:   "test",
		UserID:    "user-1",
		SessionID: "legacy-1",
	})
	if err != nil {
		t.Fatalf("create legacy session error = %v", err)
	}

	page, err := manager.ListConversations(ctx, "user-1", "", 20)
	if err != nil {
		t.Fatalf("ListConversations() error = %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ConversationID != "explicit-1" {
		t.Fatalf("ListConversations() = %v, want only explicit-1", conversationIDs(page.Items))
	}
	if err := manager.OwnsConversation(ctx, "user-1", "auto-1"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("OwnsConversation(auto) error = %v, want ErrConversationNotFound", err)
	}
	if err := manager.DeleteConversation(ctx, "user-1", "auto-1"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("DeleteConversation(auto) error = %v, want ErrConversationNotFound", err)
	}
}

func TestCreateConversationRejectsEmptyUser(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t)

	if _, err := manager.CreateConversation(ctx, "", "conversation-1"); !errors.Is(err, ErrInvalidUserID) {
		t.Fatalf("CreateConversation(empty user) error = %v, want ErrInvalidUserID", err)
	}
	if _, err := manager.convRepo.Get(ctx, "test", "", "conversation-1"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("empty-user create left metadata behind, Get error = %v", err)
	}
}

func TestGetOrCreateRequiresAutoMode(t *testing.T) {
	manager := newTestManager(t) // autoMode 默认关闭

	if _, err := manager.GetOrCreate(context.Background(), "user-1"); !errors.Is(err, ErrAutoModeUnavailable) {
		t.Fatalf("GetOrCreate(auto disabled) error = %v, want ErrAutoModeUnavailable", err)
	}
}

func TestGetOrCreateReusesAndRotates(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t)
	manager.autoMode = true

	first, err := manager.GetOrCreate(ctx, "user-1")
	if err != nil {
		t.Fatalf("GetOrCreate() error = %v", err)
	}
	if first == "" {
		t.Fatal("GetOrCreate() returned empty id")
	}

	second, err := manager.GetOrCreate(ctx, "user-1")
	if err != nil {
		t.Fatalf("GetOrCreate() second error = %v", err)
	}
	if second != first {
		t.Fatalf("GetOrCreate() reused id = %q, want %q", second, first)
	}

	// 将指针时间调整到前一个自然日，使下次调用触发轮换。
	repo := manager.convRepo.(*gormConversationRepository)
	if err := repo.db.WithContext(ctx).Model(&autoConversation{}).
		Where("app_name = ? AND user_id = ?", "test", "user-1").
		Update("updated_at", time.Now().AddDate(0, 0, -1)).Error; err != nil {
		t.Fatalf("age auto conversation error = %v", err)
	}
	rotated, err := manager.GetOrCreate(ctx, "user-1")
	if err != nil {
		t.Fatalf("GetOrCreate() after day boundary error = %v", err)
	}
	if rotated == first {
		t.Fatalf("GetOrCreate() did not rotate across day boundary, id = %q", rotated)
	}
}

type blockingCreateService struct {
	adksession.Service
	started chan struct{}
	release chan struct{}
	once    sync.Once
	creates atomic.Int32
}

func (s *blockingCreateService) Create(ctx context.Context, req *adksession.CreateRequest) (*adksession.CreateResponse, error) {
	s.creates.Add(1)
	s.once.Do(func() { close(s.started) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
		return s.Service.Create(ctx, req)
	}
}

func TestGetOrCreateCreatesOnceConcurrently(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t)
	service := &blockingCreateService{
		Service: adksession.InMemoryService(),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	manager.service = service
	manager.autoMode = true

	results := make(chan string, 2)
	errors := make(chan error, 2)
	call := func() {
		id, err := manager.GetOrCreate(ctx, "user-1")
		results <- id
		errors <- err
	}
	go call()
	<-service.started
	go call()
	close(service.release)

	first, second := <-results, <-results
	if err := <-errors; err != nil {
		t.Fatalf("GetOrCreate() error = %v", err)
	}
	if err := <-errors; err != nil {
		t.Fatalf("GetOrCreate() error = %v", err)
	}
	if first == "" || first != second {
		t.Fatalf("concurrent ids = %q and %q, want one non-empty id", first, second)
	}
	if got := service.creates.Load(); got != 1 {
		t.Fatalf("session creates = %d, want 1", got)
	}
}

func TestAutoClaimLeaseCanBeTakenOver(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t)
	now := time.Now()

	if id, claimed, err := manager.convRepo.ClaimAuto(
		ctx, "test", "user-1", "old-token", "old-candidate", now, time.Time{}, now.Add(time.Minute),
	); err != nil || !claimed || id != "old-candidate" {
		t.Fatalf("first ClaimAuto() = id %q, claimed %v, error %v", id, claimed, err)
	}
	if id, claimed, err := manager.convRepo.ClaimAuto(
		ctx, "test", "user-1", "new-token", "new-candidate", now.Add(2*time.Minute), time.Time{}, now.Add(3*time.Minute),
	); err != nil || !claimed || id != "old-candidate" {
		t.Fatalf("takeover ClaimAuto() = id %q, claimed %v, error %v", id, claimed, err)
	}
	if err := manager.convRepo.CompleteAuto(ctx, "test", "user-1", "old-token", "old-session", now); err == nil {
		t.Fatal("old CompleteAuto() error = nil after lease takeover")
	}
	if err := manager.convRepo.CompleteAuto(ctx, "test", "user-1", "new-token", "old-candidate", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("new CompleteAuto() error = %v", err)
	}

	manager.autoMode = true
	id, err := manager.GetOrCreate(ctx, "user-1")
	if err != nil {
		t.Fatalf("GetOrCreate() error = %v", err)
	}
	if id != "old-candidate" {
		t.Fatalf("GetOrCreate() id = %q, want old-candidate", id)
	}
}

func TestGetOrCreateRecoversCreatedSessionOnTakeover(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t)
	manager.autoMode = true
	now := time.Now()

	// 前一个创建者完成 claim 和会话创建后，在完成元数据前崩溃：
	// 此时会留下过期的 creating claim 和待恢复会话。
	if _, claimed, err := manager.convRepo.ClaimAuto(
		ctx, "test", "user-1", "crashed-token", "orphan-session", now, time.Time{}, now.Add(-time.Second),
	); err != nil || !claimed {
		t.Fatalf("seed claim: claimed %v err %v", claimed, err)
	}
	if _, err := manager.service.Create(ctx, &adksession.CreateRequest{
		AppName: "test", UserID: "user-1", SessionID: "orphan-session",
		State: map[string]any{sessionModeKey: sessionModeAuto},
	}); err != nil {
		t.Fatalf("seed orphan session: %v", err)
	}

	id, err := manager.GetOrCreate(ctx, "user-1")
	if err != nil {
		t.Fatalf("GetOrCreate() error = %v", err)
	}
	if id != "orphan-session" {
		t.Fatalf("GetOrCreate() id = %q, want orphan-session", id)
	}
	if _, err := manager.service.Get(ctx, &adksession.GetRequest{
		AppName: "test", UserID: "user-1", SessionID: "orphan-session",
	}); err != nil {
		t.Fatalf("recovered session is missing: %v", err)
	}
}

type completeResponseLostRepository struct {
	ConversationRepository
	lostID string
	lost   bool
	calls  int
}

func (r *completeResponseLostRepository) CompleteAuto(ctx context.Context, appName, userID, token, conversationID string, now time.Time) error {
	r.calls++
	if r.lost {
		return r.ConversationRepository.CompleteAuto(ctx, appName, userID, token, conversationID, now)
	}
	r.lost = true
	r.lostID = conversationID
	if err := r.ConversationRepository.CompleteAuto(ctx, appName, userID, token, conversationID, now); err != nil {
		return err
	}
	return errAutoConversationClaimLost
}

func TestGetOrCreateRecoversAfterCompleteResponseLoss(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t)
	repo := &completeResponseLostRepository{ConversationRepository: manager.convRepo}
	manager.convRepo = repo
	manager.autoMode = true

	id, err := manager.GetOrCreate(ctx, "user-1")
	if err != nil {
		t.Fatalf("GetOrCreate() error = %v", err)
	}
	if id == "" || id != repo.lostID {
		t.Fatalf("GetOrCreate() id = %q, want %q (CompleteAuto calls: %d)", id, repo.lostID, repo.calls)
	}
	if repo.lostID == "" {
		t.Fatal("claim loss did not capture created session")
	}
	if _, err := manager.service.Get(ctx, &adksession.GetRequest{
		AppName: "test", UserID: "user-1", SessionID: repo.lostID,
	}); err != nil {
		t.Fatalf("session missing after completion response loss: %v", err)
	}
}

type committedCreateErrorService struct {
	adksession.Service
	createdID string
}

func (s *committedCreateErrorService) Create(ctx context.Context, req *adksession.CreateRequest) (*adksession.CreateResponse, error) {
	s.createdID = req.SessionID
	if _, err := s.Service.Create(ctx, req); err != nil {
		return nil, err
	}
	return nil, errors.New("connection reset after commit")
}

func TestGetOrCreateRecoversCommittedCreateError(t *testing.T) {
	manager := newTestManager(t)
	service := &committedCreateErrorService{Service: adksession.InMemoryService()}
	manager.service = service
	manager.autoMode = true

	id, err := manager.GetOrCreate(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("GetOrCreate() error = %v", err)
	}
	if id == "" {
		t.Fatal("GetOrCreate() returned empty id")
	}
	if id != service.createdID {
		t.Fatalf("GetOrCreate() id = %q, created id = %q", id, service.createdID)
	}
	if _, err := manager.service.Get(context.Background(), &adksession.GetRequest{
		AppName: "test", UserID: "user-1", SessionID: id,
	}); err != nil {
		t.Fatalf("committed session is missing: %v", err)
	}
}

func TestCreateConversationDoesNotDeleteConflictingSession(t *testing.T) {
	ctx := context.Background()
	dialector := sqlite.Open("file:conflict-" + t.Name() + "?mode=memory&cache=shared&_busy_timeout=5000")
	manager, err := New("app", dialector, WithReconcileInterval(0))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer manager.Close()

	// 一个未纳入元数据管理的会话已占用此 ID，例如通过 Service 在外部创建。
	if _, err := manager.Service().Create(ctx, &adksession.CreateRequest{
		AppName: "app", UserID: "user-1", SessionID: "dup-1",
	}); err != nil {
		t.Fatalf("seed session error = %v", err)
	}

	if _, err := manager.CreateConversation(ctx, "user-1", "dup-1"); err == nil {
		t.Fatal("CreateConversation() error = nil, want duplicate failure")
	}

	// 创建失败后，原有会话必须继续存在。
	if _, err := manager.Service().Get(ctx, &adksession.GetRequest{
		AppName: "app", UserID: "user-1", SessionID: "dup-1",
	}); err != nil {
		t.Fatalf("conflicting session was deleted: %v", err)
	}
	metadata, err := manager.convRepo.Get(ctx, "app", "user-1", "dup-1")
	if err != nil {
		t.Fatalf("Get metadata error = %v", err)
	}
	if metadata.Status != ConversationFailed {
		t.Fatalf("metadata status = %q, want failed", metadata.Status)
	}
}

func conversationIDs(conversations []Conversation) []string {
	ids := make([]string, 0, len(conversations))
	for _, conversation := range conversations {
		ids = append(ids, conversation.ConversationID)
	}
	return ids
}
