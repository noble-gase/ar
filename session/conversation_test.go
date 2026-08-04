package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"
	"gorm.io/driver/sqlite"
)

func newTestRepository(t *testing.T) ConversationRepository {
	t.Helper()

	repo, err := NewConversationRepository(sqlite.Open("file:repo-" + t.Name() + "?mode=memory&cache=shared"))
	if err != nil {
		t.Fatalf("create conversation repository: %v", err)
	}
	return repo
}

func TestConversationRepositoryOwnershipAndPagination(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	base := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)

	for index, id := range []string{"first", "second", "third"} {
		err := repo.Create(ctx, &Conversation{
			AppName: "app", UserID: "user-1", ConversationID: id,
			Status: ConversationActive, Version: 1,
			CreatedAt: base, UpdatedAt: base.Add(time.Duration(index) * time.Minute),
		})
		if err != nil {
			t.Fatalf("Create(%s) error = %v", id, err)
		}
	}
	if err := repo.Create(ctx, &Conversation{
		AppName: "app", UserID: "user-2", ConversationID: "other",
		Status: ConversationActive, Version: 1, CreatedAt: base, UpdatedAt: base,
	}); err != nil {
		t.Fatalf("Create(other) error = %v", err)
	}

	if err := repo.Owns(ctx, "app", "user-1", "third"); err != nil {
		t.Fatalf("Owns() error = %v", err)
	}
	if err := repo.Owns(ctx, "app", "user-2", "third"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("Owns(wrong user) error = %v, want ErrConversationNotFound", err)
	}

	firstPage, err := repo.List(ctx, "app", "user-1", "", 2)
	if err != nil {
		t.Fatalf("List(first page) error = %v", err)
	}
	if got := conversationIDs(firstPage.Items); fmt.Sprint(got) != "[third second]" {
		t.Fatalf("List(first page) = %v, want [third second]", got)
	}
	if firstPage.NextCursor == "" {
		t.Fatal("List(first page) returned empty cursor")
	}

	secondPage, err := repo.List(ctx, "app", "user-1", firstPage.NextCursor, 2)
	if err != nil {
		t.Fatalf("List(second page) error = %v", err)
	}
	if got := conversationIDs(secondPage.Items); fmt.Sprint(got) != "[first]" {
		t.Fatalf("List(second page) = %v, want [first]", got)
	}
	if secondPage.NextCursor != "" {
		t.Fatalf("List(second page) cursor = %q, want empty", secondPage.NextCursor)
	}
}

func TestConversationRepositoryTouchAndDeleteState(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	base := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	conversation := &Conversation{
		AppName: "app", UserID: "user-1", ConversationID: "conversation-1",
		Status: ConversationActive, Version: 1, CreatedAt: base, UpdatedAt: base,
	}
	if err := repo.Create(ctx, conversation); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := repo.Touch(ctx, "app", "user-1", "conversation-1", base.Add(time.Hour)); err != nil {
		t.Fatalf("Touch() error = %v", err)
	}
	got, err := repo.Get(ctx, "app", "user-1", "conversation-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Version != 2 || !got.UpdatedAt.Equal(base.Add(time.Hour)) {
		t.Fatalf("Touch() metadata = version %d, updatedAt %v", got.Version, got.UpdatedAt)
	}

	if err := repo.BeginDelete(ctx, "app", "user-1", "conversation-1"); err != nil {
		t.Fatalf("BeginDelete() error = %v", err)
	}
	if err := repo.BeginDelete(ctx, "app", "user-1", "conversation-1"); err != nil {
		t.Fatalf("BeginDelete() retry error = %v", err)
	}
	if err := repo.Owns(ctx, "app", "user-1", "conversation-1"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("Owns(deleting) error = %v, want ErrConversationNotFound", err)
	}
}

type failingSessionService struct {
	adksession.Service
	createErr error
	deleteErr error
}

func (s *failingSessionService) Create(ctx context.Context, req *adksession.CreateRequest) (*adksession.CreateResponse, error) {
	if s.createErr != nil {
		return nil, s.createErr
	}
	return s.Service.Create(ctx, req)
}

func (s *failingSessionService) Delete(ctx context.Context, req *adksession.DeleteRequest) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.Service.Delete(ctx, req)
}

func TestCreateConversationCompensatesMetadata(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	manager := &Session{
		name: "app", convRepo: repo,
		service: &failingSessionService{Service: adksession.InMemoryService(), createErr: errors.New("create failed")},
	}

	if _, err := manager.CreateConversation(ctx, "user-1", "conversation-1"); err == nil {
		t.Fatal("CreateConversation() error = nil, want failure")
	}
	metadata, err := repo.Get(ctx, "app", "user-1", "conversation-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if metadata.Status != ConversationFailed {
		t.Fatalf("metadata status = %q, want failed", metadata.Status)
	}
}

func TestDeleteConversationKeepsDeletingOnFailure(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	service := &failingSessionService{Service: adksession.InMemoryService()}
	manager := &Session{name: "app", convRepo: repo, service: service}

	if _, err := manager.CreateConversation(ctx, "user-1", "conversation-1"); err != nil {
		t.Fatalf("CreateConversation() error = %v", err)
	}
	service.deleteErr = errors.New("delete failed")
	if err := manager.DeleteConversation(ctx, "user-1", "conversation-1"); err == nil {
		t.Fatal("DeleteConversation() error = nil, want failure")
	}
	metadata, err := repo.Get(ctx, "app", "user-1", "conversation-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if metadata.Status != ConversationDeleting {
		t.Fatalf("metadata status = %q, want deleting", metadata.Status)
	}
}

func TestReconcileConversationsCompletesInterruptedOperations(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	service := adksession.InMemoryService()
	manager := &Session{name: "app", convRepo: repo, service: service}
	now := time.Now()

	for _, item := range []struct {
		id     string
		status ConversationStatus
	}{
		{id: "creating-1", status: ConversationCreating},
		{id: "deleting-1", status: ConversationDeleting},
	} {
		if err := repo.Create(ctx, &Conversation{
			AppName: "app", UserID: "user-1", ConversationID: item.id,
			Status: item.status, Version: 1, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("Create(%s) metadata error = %v", item.id, err)
		}
		if _, err := service.Create(ctx, &adksession.CreateRequest{
			AppName: "app", UserID: "user-1", SessionID: item.id,
		}); err != nil {
			t.Fatalf("Create(%s) session error = %v", item.id, err)
		}
	}

	if err := manager.ReconcileConversations(ctx); err != nil {
		t.Fatalf("ReconcileConversations() error = %v", err)
	}
	creating, err := repo.Get(ctx, "app", "user-1", "creating-1")
	if err != nil {
		t.Fatalf("Get(creating-1) error = %v", err)
	}
	if creating.Status != ConversationActive {
		t.Fatalf("creating status = %q, want active", creating.Status)
	}
	deleting, err := repo.Get(ctx, "app", "user-1", "deleting-1")
	if err != nil {
		t.Fatalf("Get(deleting-1) error = %v", err)
	}
	if deleting.Status != ConversationDeleted {
		t.Fatalf("deleting status = %q, want deleted", deleting.Status)
	}
	if _, err := service.Get(ctx, &adksession.GetRequest{
		AppName: "app", UserID: "user-1", SessionID: "deleting-1",
	}); err == nil {
		t.Fatal("deleting session still exists after reconciliation")
	}
}

func TestNewInitializesConversationMetadata(t *testing.T) {
	dialector := sqlite.Open("file:new-" + t.Name() + "?mode=memory&cache=shared")
	manager, err := New("app", dialector, WithReconcileInterval(0))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer manager.Close()
	if _, err := manager.CreateConversation(context.Background(), "user-1", "conversation-1"); err != nil {
		t.Fatalf("CreateConversation() error = %v", err)
	}
	if err := manager.OwnsConversation(context.Background(), "user-1", "conversation-1"); err != nil {
		t.Fatalf("OwnsConversation() error = %v", err)
	}
}

func TestNewSharesBareSQLiteMemoryConnection(t *testing.T) {
	manager, err := New("app", sqlite.Open(":memory:"), WithReconcileInterval(0))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer manager.Close()

	ctx := context.Background()
	if _, err := manager.CreateConversation(ctx, "user-1", "conversation-1"); err != nil {
		t.Fatalf("CreateConversation() error = %v", err)
	}
	if err := manager.OwnsConversation(ctx, "user-1", "conversation-1"); err != nil {
		t.Fatalf("OwnsConversation() error = %v", err)
	}
	if _, err := manager.GetConversation(ctx, "user-1", "conversation-1"); err != nil {
		t.Fatalf("GetConversation() error = %v", err)
	}
}

func TestBackgroundReconcilerCompletesDeleteAndCloses(t *testing.T) {
	manager, err := New("app", sqlite.Open("file:reconciler-"+t.Name()+"?mode=memory&cache=shared"), WithReconcileInterval(10*time.Millisecond))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()
	if _, err := manager.CreateConversation(ctx, "user-1", "conversation-1"); err != nil {
		t.Fatalf("CreateConversation() error = %v", err)
	}
	if err := manager.convRepo.BeginDelete(ctx, "app", "user-1", "conversation-1"); err != nil {
		t.Fatalf("BeginDelete() error = %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		conversation, getErr := manager.convRepo.Get(ctx, "app", "user-1", "conversation-1")
		if getErr != nil {
			t.Fatalf("Get() error = %v", getErr)
		}
		if conversation.Status == ConversationDeleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("metadata status = %q, want deleted", conversation.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestReconcileConversationsFailsOrphanedCreate(t *testing.T) {
	ctx := context.Background()
	dialector := sqlite.Open("file:orphan-reconcile-" + t.Name() + "?mode=memory&cache=shared")
	manager, err := New("app", dialector, WithReconcileInterval(0))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer manager.Close()
	now := time.Now()
	if err := manager.convRepo.Create(ctx, &Conversation{
		AppName: "app", UserID: "user-1", ConversationID: "orphan",
		Status: ConversationCreating, Version: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Create orphan metadata error = %v", err)
	}
	if err := manager.ReconcileConversations(ctx); err != nil {
		t.Fatalf("ReconcileConversations() error = %v", err)
	}
	orphan, err := manager.convRepo.Get(ctx, "app", "user-1", "orphan")
	if err != nil {
		t.Fatalf("Get(orphan) error = %v", err)
	}
	if orphan.Status != ConversationFailed {
		t.Fatalf("orphan status = %q, want failed", orphan.Status)
	}
}

// racingReconcilerRepository 模拟状态转换已提交但响应丢失的场景。
type racingReconcilerRepository struct {
	ConversationRepository
}

func (r *racingReconcilerRepository) Transition(ctx context.Context, appName, userID, conversationID string, from, to ConversationStatus) error {
	if from == ConversationCreating && to == ConversationActive {
		if err := r.ConversationRepository.Transition(ctx, appName, userID, conversationID, from, to); err != nil {
			return err
		}
		return errors.New("connection reset after commit")
	}
	return r.ConversationRepository.Transition(ctx, appName, userID, conversationID, from, to)
}

func TestCreateConversationToleratesReconcilerRace(t *testing.T) {
	ctx := context.Background()
	service := adksession.InMemoryService()
	manager := &Session{
		name:     "app",
		convRepo: &racingReconcilerRepository{ConversationRepository: newTestRepository(t)},
		service:  service,
	}

	created, err := manager.CreateConversation(ctx, "user-1", "conversation-1")
	if err != nil {
		t.Fatalf("CreateConversation() error = %v, want nil despite reconciler race", err)
	}
	if created == nil || created.ID() != "conversation-1" {
		t.Fatalf("CreateConversation() session = %v, want conversation-1", created)
	}
	// ADK 会话必须继续存在，不能执行错误的补偿删除。
	if _, err := service.Get(ctx, &adksession.GetRequest{
		AppName: "app", UserID: "user-1", SessionID: "conversation-1",
	}); err != nil {
		t.Fatalf("session missing after create race, Get error = %v", err)
	}
	if err := manager.OwnsConversation(ctx, "user-1", "conversation-1"); err != nil {
		t.Fatalf("OwnsConversation() error = %v", err)
	}
}

// failingActivateRepository 模拟 creating 到 active 的转换未提交便失败，
// 记录会保留在 creating，随后必须由协调器推进状态。
type failingActivateRepository struct {
	ConversationRepository
}

func (r *failingActivateRepository) Transition(ctx context.Context, appName, userID, conversationID string, from, to ConversationStatus) error {
	if from == ConversationCreating && to == ConversationActive {
		return errors.New("activate transition failed")
	}
	return r.ConversationRepository.Transition(ctx, appName, userID, conversationID, from, to)
}

func TestCreateConversationKeepsSessionOnActivateFailure(t *testing.T) {
	ctx := context.Background()
	service := adksession.InMemoryService()
	repo := newTestRepository(t)
	manager := &Session{
		name:     "app",
		convRepo: &failingActivateRepository{ConversationRepository: repo},
		service:  service,
	}

	if _, err := manager.CreateConversation(ctx, "user-1", "conversation-1"); err == nil {
		t.Fatal("CreateConversation() error = nil, want activate failure")
	}
	// 不能执行补偿删除，已创建的会话必须保留给协调器处理。
	if _, err := service.Get(ctx, &adksession.GetRequest{
		AppName: "app", UserID: "user-1", SessionID: "conversation-1",
	}); err != nil {
		t.Fatalf("session deleted after activate failure, Get error = %v", err)
	}
	metadata, err := repo.Get(ctx, "app", "user-1", "conversation-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if metadata.Status != ConversationCreating {
		t.Fatalf("metadata status = %q, want creating", metadata.Status)
	}

	// 协调器将中断后遗留的 creating 记录推进为 active。
	recovered := &Session{name: "app", convRepo: repo, service: service}
	if err := recovered.ReconcileConversations(ctx); err != nil {
		t.Fatalf("ReconcileConversations() error = %v", err)
	}
	if err := recovered.OwnsConversation(ctx, "user-1", "conversation-1"); err != nil {
		t.Fatalf("OwnsConversation() after reconcile error = %v", err)
	}
}

type selectiveGetFailureService struct {
	adksession.Service
	failingPrefix string
}

func (s *selectiveGetFailureService) Get(ctx context.Context, req *adksession.GetRequest) (*adksession.GetResponse, error) {
	if strings.HasPrefix(req.SessionID, s.failingPrefix) {
		return nil, errors.New("permanent get failure")
	}
	return s.Service.Get(ctx, req)
}

func TestReconcileConversationsDoesNotStarveLaterRecords(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	service := &selectiveGetFailureService{
		Service: adksession.InMemoryService(), failingPrefix: "blocked-",
	}
	manager := &Session{name: "app", convRepo: repo, service: service}
	base := time.Now().Add(-time.Hour)

	for index := range 100 {
		if err := repo.Create(ctx, &Conversation{
			AppName: "app", UserID: "user-1", ConversationID: fmt.Sprintf("blocked-%03d", index),
			Status: ConversationCreating, Version: 1, CreatedAt: base, UpdatedAt: base,
		}); err != nil {
			t.Fatalf("Create blocked metadata error = %v", err)
		}
	}
	if err := repo.Create(ctx, &Conversation{
		AppName: "app", UserID: "user-1", ConversationID: "recoverable",
		Status: ConversationCreating, Version: 1, CreatedAt: base, UpdatedAt: base.Add(time.Minute),
	}); err != nil {
		t.Fatalf("Create recoverable metadata error = %v", err)
	}
	if _, err := service.Create(ctx, &adksession.CreateRequest{
		AppName: "app", UserID: "user-1", SessionID: "recoverable",
	}); err != nil {
		t.Fatalf("Create recoverable session error = %v", err)
	}

	if err := manager.ReconcileConversations(ctx); err == nil {
		t.Fatal("first ReconcileConversations() error = nil, want blocked failures")
	}
	if err := manager.ReconcileConversations(ctx); err == nil {
		t.Fatal("second ReconcileConversations() error = nil, want remaining blocked failures")
	}
	recovered, err := repo.Get(ctx, "app", "user-1", "recoverable")
	if err != nil {
		t.Fatalf("Get(recoverable) error = %v", err)
	}
	if recovered.Status != ConversationActive {
		t.Fatalf("recoverable status = %q, want active", recovered.Status)
	}
}

func TestReconcileConversationsStopsAfterMaxAttempts(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	service := &selectiveGetFailureService{
		Service: adksession.InMemoryService(), failingPrefix: "blocked-",
	}
	manager := &Session{name: "app", convRepo: repo, service: service}
	now := time.Now()
	if err := repo.Create(ctx, &Conversation{
		AppName: "app", UserID: "user-1", ConversationID: "blocked-1",
		Status: ConversationCreating, Version: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	for attempt := 1; attempt <= maxReconcileAttempts; attempt++ {
		if err := manager.ReconcileConversations(ctx); err == nil {
			t.Fatalf("ReconcileConversations() attempt %d error = nil", attempt)
		}
	}
	conversation, err := repo.Get(ctx, "app", "user-1", "blocked-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if conversation.Status != ConversationFailed {
		t.Fatalf("status = %q, want failed", conversation.Status)
	}
	if conversation.ReconcileAttempts != maxReconcileAttempts {
		t.Fatalf("reconcile attempts = %d, want %d", conversation.ReconcileAttempts, maxReconcileAttempts)
	}
	if conversation.LastReconcileError == "" {
		t.Fatal("last reconcile error is empty")
	}
	if err := manager.ReconcileConversations(ctx); err != nil {
		t.Fatalf("ReconcileConversations() after terminal failure error = %v", err)
	}
}

func TestReconcileDeletingNeverBecomesTerminal(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	service := &failingSessionService{
		Service: adksession.InMemoryService(), deleteErr: errors.New("delete failed"),
	}
	manager := &Session{name: "app", convRepo: repo, service: service}
	now := time.Now()
	if err := repo.Create(ctx, &Conversation{
		AppName: "app", UserID: "user-1", ConversationID: "deleting-1",
		Status: ConversationDeleting, Version: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// 即使超过 creating 的重试上限，deleting 会话也必须继续重试。
	for attempt := 1; attempt <= maxReconcileAttempts+2; attempt++ {
		if err := manager.ReconcileConversations(ctx); err == nil {
			t.Fatalf("ReconcileConversations() attempt %d error = nil", attempt)
		}
	}
	conversation, err := repo.Get(ctx, "app", "user-1", "deleting-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if conversation.Status != ConversationDeleting {
		t.Fatalf("status = %q, want deleting", conversation.Status)
	}
	pending, err := repo.Pending(ctx, "app", 10)
	if err != nil {
		t.Fatalf("Pending() error = %v", err)
	}
	if len(pending) != 1 || pending[0].ConversationID != "deleting-1" {
		t.Fatalf("pending = %v, want [deleting-1] still retryable", conversationIDs(pending))
	}
}

func TestTransitionClearsReconcileFailure(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	now := time.Now()
	if err := repo.Create(ctx, &Conversation{
		AppName: "app", UserID: "user-1", ConversationID: "conversation-1",
		Status: ConversationCreating, Version: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := repo.RecordReconcileFailure(
		ctx, "app", "user-1", "conversation-1", ConversationCreating, "temporary failure", maxReconcileAttempts, now,
	); err != nil {
		t.Fatalf("RecordReconcileFailure() error = %v", err)
	}
	if err := repo.Transition(ctx, "app", "user-1", "conversation-1", ConversationCreating, ConversationActive); err != nil {
		t.Fatalf("Transition() error = %v", err)
	}
	conversation, err := repo.Get(ctx, "app", "user-1", "conversation-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if conversation.ReconcileAttempts != 0 || conversation.LastReconcileError != "" {
		t.Fatalf("reconcile failure was not cleared: attempts=%d error=%q", conversation.ReconcileAttempts, conversation.LastReconcileError)
	}
}
