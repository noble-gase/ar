package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

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

	created, err := manager.Create(ctx, "user-1", "conversation-1")
	if err != nil {
		t.Fatalf("CreateConversation() error = %v", err)
	}
	if created.ID() != "conversation-1" {
		t.Fatalf("CreateConversation() ID = %q, want %q", created.ID(), "conversation-1")
	}

	got, err := manager.Get(ctx, "user-1", "conversation-1")
	if err != nil {
		t.Fatalf("GetConversation() error = %v", err)
	}
	if got.UserID() != "user-1" {
		t.Fatalf("GetConversation() UserID = %q, want %q", got.UserID(), "user-1")
	}

	page, err := manager.List(ctx, "user-1", "", 20)
	if err != nil {
		t.Fatalf("ListConversations() error = %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("ListConversations() length = %d, want 1", len(page.Items))
	}

	if _, err := manager.Get(ctx, "user-2", "conversation-1"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("GetConversation() wrong owner error = %v, want ErrConversationNotFound", err)
	}

	if err := manager.Delete(ctx, "user-1", "conversation-1"); err != nil {
		t.Fatalf("DeleteConversation() error = %v", err)
	}
	if _, err := manager.Get(ctx, "user-1", "conversation-1"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("GetConversation() after delete error = %v, want ErrConversationNotFound", err)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	service := &failingSessionService{Service: adksession.InMemoryService()}
	manager := &Session{name: "app", convRepo: repo, service: service}

	if _, err := manager.Create(ctx, "user-1", "conversation-1"); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	// ADK 删除失败不影响调用方结果：元数据已删除，会话对用户已经消失。
	service.deleteErr = errors.New("delete failed")
	if err := manager.Delete(ctx, "user-1", "conversation-1"); err != nil {
		t.Fatalf("Delete() error = %v, want nil", err)
	}
	if _, err := repo.Get(ctx, "app", "user-1", "conversation-1"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("Get() after delete error = %v, want ErrConversationNotFound", err)
	}
	if err := manager.Delete(ctx, "user-1", "conversation-1"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("Delete() repeat error = %v, want ErrConversationNotFound", err)
	}
}

func TestCreateConversationGeneratesID(t *testing.T) {
	manager := newTestManager(t)

	created, err := manager.Create(context.Background(), "user-1", "")
	if err != nil {
		t.Fatalf("CreateConversation() error = %v", err)
	}
	if created.ID() == "" {
		t.Fatal("CreateConversation() generated an empty ID")
	}
}

func TestRenameConversation(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t)

	if _, err := manager.Create(ctx, "user-1", "conversation-1"); err != nil {
		t.Fatalf("CreateConversation() error = %v", err)
	}
	before, err := manager.GetMeta(ctx, "user-1", "conversation-1")
	if err != nil {
		t.Fatalf("GetConversationMeta() error = %v", err)
	}
	if err := manager.Rename(ctx, "user-1", "conversation-1", "季度报告"); err != nil {
		t.Fatalf("RenameConversation() error = %v", err)
	}
	metadata, err := manager.GetMeta(ctx, "user-1", "conversation-1")
	if err != nil {
		t.Fatalf("GetConversationMeta() error = %v", err)
	}
	if metadata.Title != "季度报告" {
		t.Fatalf("title = %q, want 季度报告", metadata.Title)
	}
	// 重命名不能改变按活跃度排序的列表位置。
	if !metadata.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("rename bumped updatedAt: %v -> %v", before.UpdatedAt, metadata.UpdatedAt)
	}

	if err := manager.Rename(ctx, "user-2", "conversation-1", "别人的"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("RenameConversation(wrong owner) error = %v, want ErrConversationNotFound", err)
	}
	if err := manager.Rename(
		ctx, "user-1", "conversation-1", strings.Repeat("x", maxTitleRunes+1),
	); !errors.Is(err, ErrTitleTooLong) {
		t.Fatalf("RenameConversation(long title) error = %v, want ErrTitleTooLong", err)
	}
}

func TestConversationModesAreIsolated(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t)
	service := manager.service

	if _, err := manager.Create(ctx, "user-1", "explicit-1"); err != nil {
		t.Fatalf("CreateConversation() error = %v", err)
	}

	// 自动会话不写入元数据表，因此对显式会话接口不可见。
	if _, err := service.Create(ctx, &adksession.CreateRequest{
		AppName: "test", UserID: "user-1", SessionID: "auto-1",
	}); err != nil {
		t.Fatalf("create auto session error = %v", err)
	}

	page, err := manager.List(ctx, "user-1", "", 20)
	if err != nil {
		t.Fatalf("ListConversations() error = %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ConversationID != "explicit-1" {
		t.Fatalf("ListConversations() = %v, want only explicit-1", conversationIDs(page.Items))
	}
	if _, err := manager.GetMeta(ctx, "user-1", "auto-1"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("GetConversationMeta(auto) error = %v, want ErrConversationNotFound", err)
	}
	if err := manager.Delete(ctx, "user-1", "auto-1"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("DeleteConversation(auto) error = %v, want ErrConversationNotFound", err)
	}
}

func TestEmptyUserIsRejectedBeforeAnyWrite(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t)

	if _, err := manager.Create(ctx, "", "conversation-1"); !errors.Is(err, ErrInvalidUserID) {
		t.Fatalf("Create(empty user) error = %v, want ErrInvalidUserID", err)
	}
	// 空 userId 的元数据无法被协调器裁决（ADK Get 同样报参数错误），绝不能落库。
	if _, err := manager.convRepo.Get(ctx, "test", "", "conversation-1"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("empty-user create left metadata behind, Get error = %v", err)
	}
	if _, err := manager.GetOrCreate(ctx, "", 1); !errors.Is(err, ErrInvalidUserID) {
		t.Fatalf("GetOrCreate(empty user) error = %v, want ErrInvalidUserID", err)
	}
}

func TestGetOrCreateReusesPerUser(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t)

	first, err := manager.GetOrCreate(ctx, "user-1", 1)
	if err != nil {
		t.Fatalf("GetOrCreate() error = %v", err)
	}
	if first.ID() == "" {
		t.Fatal("GetOrCreate() returned empty id")
	}

	second, err := manager.GetOrCreate(ctx, "user-1", 1)
	if err != nil {
		t.Fatalf("GetOrCreate() second error = %v", err)
	}
	if second.ID() != first.ID() {
		t.Fatalf("GetOrCreate() reused id = %q, want %q", second.ID(), first.ID())
	}

	other, err := manager.GetOrCreate(ctx, "user-2", 1)
	if err != nil {
		t.Fatalf("GetOrCreate(user-2) error = %v", err)
	}
	if other.ID() == first.ID() {
		t.Fatalf("GetOrCreate(user-2) id = %q, want a different id", other.ID())
	}
}

func TestGetOrCreateCreatesOnceConcurrently(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t)

	const callers = 8
	ids := make(chan string, callers)
	errs := make(chan error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	start := make(chan struct{})
	for range callers {
		go func() {
			ready.Done()
			<-start
			conversation, err := manager.GetOrCreate(ctx, "user-1", 1)
			if err != nil {
				ids <- ""
				errs <- err
				return
			}
			ids <- conversation.ID()
			errs <- nil
		}()
	}
	ready.Wait()
	close(start)

	want := autoConversationID("test", "user-1")
	for range callers {
		if err := <-errs; err != nil {
			t.Fatalf("GetOrCreate() error = %v", err)
		}
		if id := <-ids; id != want {
			t.Fatalf("concurrent id = %q, want %q", id, want)
		}
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

	conversation, err := manager.GetOrCreate(context.Background(), "user-1", 1)
	if err != nil {
		t.Fatalf("GetOrCreate() error = %v", err)
	}
	if conversation.ID() != service.createdID {
		t.Fatalf("GetOrCreate() id = %q, created id = %q", conversation.ID(), service.createdID)
	}
	if _, err := manager.service.Get(context.Background(), &adksession.GetRequest{
		AppName: "test", UserID: "user-1", SessionID: conversation.ID(),
	}); err != nil {
		t.Fatalf("committed session is missing: %v", err)
	}
}

func TestCreateConversationDoesNotDeleteConflictingSession(t *testing.T) {
	ctx := context.Background()
	dialector := sqlite.Open("file:conflict-" + t.Name() + "?mode=memory&cache=shared&_busy_timeout=5000")
	manager, err := New("app", dialector)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// 一个未纳入元数据管理的会话已占用此 ID，例如通过 Service 在外部创建。
	if _, err := manager.Service().Create(ctx, &adksession.CreateRequest{
		AppName: "app", UserID: "user-1", SessionID: "dup-1",
	}); err != nil {
		t.Fatalf("seed session error = %v", err)
	}

	if _, err := manager.Create(ctx, "user-1", "dup-1"); err == nil {
		t.Fatal("Create() error = nil, want duplicate failure")
	}

	// 创建失败后，原有会话必须继续存在。
	if _, err := manager.Service().Get(ctx, &adksession.GetRequest{
		AppName: "app", UserID: "user-1", SessionID: "dup-1",
	}); err != nil {
		t.Fatalf("conflicting session was deleted: %v", err)
	}
	// 冲突发生在 ADK 侧，元数据不会被写入。
	if _, err := manager.convRepo.Get(ctx, "app", "user-1", "dup-1"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("Get metadata error = %v, want ErrConversationNotFound", err)
	}
}
