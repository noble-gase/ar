package session

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newTestRepository(t *testing.T) *conversationRepository {
	t.Helper()

	// 临时文件避免命名共享缓存库在 -count>1 时的状态残留
	dsn := "file:" + filepath.Join(t.TempDir(), "repo.db") + "?_busy_timeout=5000&_journal_mode=WAL"
	db, err := gorm.Open(sqlite.Open(dsn), gormConfig())
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.AutoMigrate(&Conversation{}); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	return &conversationRepository{db: db}
}

func TestConversationRepositoryPagination(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	base := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)

	for index, id := range []string{"first", "second", "third"} {
		err := repo.Create(ctx, &Conversation{
			AppName: "app", UserID: "user-1", ConversationID: id,
			CreatedAt: base, UpdatedAt: base.Add(time.Duration(index) * time.Minute),
		})
		if err != nil {
			t.Fatalf("Create(%s) error = %v", id, err)
		}
	}
	if err := repo.Create(ctx, &Conversation{
		AppName: "app", UserID: "user-2", ConversationID: "other",
		CreatedAt: base, UpdatedAt: base,
	}); err != nil {
		t.Fatalf("Create(other) error = %v", err)
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
	if other, err := repo.List(ctx, "app", "user-2", "", 20); err != nil {
		t.Fatalf("List(user-2) error = %v", err)
	} else if got := conversationIDs(other.Items); fmt.Sprint(got) != "[other]" {
		t.Fatalf("List(user-2) = %v, want [other]", got)
	}
}

func TestConversationRepositoryTouch(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	base := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	if err := repo.Create(ctx, &Conversation{
		AppName: "app", UserID: "user-1", ConversationID: "conversation-1",
		CreatedAt: base, UpdatedAt: base,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := repo.Touch(ctx, "app", "user-1", "conversation-1", base.Add(time.Hour)); err != nil {
		t.Fatalf("Touch() error = %v", err)
	}
	got, err := repo.Get(ctx, "app", "user-1", "conversation-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !got.UpdatedAt.Equal(base.Add(time.Hour)) {
		t.Fatalf("Touch() metadata updatedAt = %v", got.UpdatedAt)
	}
	if err := repo.Touch(ctx, "app", "user-2", "conversation-1", base.Add(time.Hour)); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("Touch(wrong user) error = %v, want ErrConversationNotFound", err)
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

func TestCreateRollsBackMetadataWhenSessionCreateFails(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	manager := &Session{
		name: "app", convRepo: repo,
		service: &failingSessionService{Service: adksession.InMemoryService(), createErr: errors.New("create failed")},
	}

	if _, err := manager.Create(ctx, "user-1", "conversation-1"); err == nil {
		t.Fatal("Create() error = nil, want failure")
	}
	// ADK 创建失败使事务回滚，不能留下任何元数据。
	if _, err := repo.Get(ctx, "app", "user-1", "conversation-1"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("Get() error = %v, want ErrConversationNotFound", err)
	}
}

func TestCreateRejectsOversizedIdsBeforeAnyWrite(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	service := adksession.InMemoryService()
	manager := &Session{name: "app", convRepo: repo, service: service}

	longUser := strings.Repeat("u", maxUserIDRunes+1)
	if _, err := manager.Create(ctx, longUser, "conversation-1"); !errors.Is(err, ErrInvalidUserID) {
		t.Fatalf("Create(long user) error = %v, want ErrInvalidUserID", err)
	}
	longConversation := strings.Repeat("c", maxConvIDRunes+1)
	if _, err := manager.Create(ctx, "user-1", longConversation); !errors.Is(err, ErrInvalidConversationID) {
		t.Fatalf("Create(long conversation) error = %v, want ErrInvalidConversationID", err)
	}

	// 超长入参若放行，ADK 会话会先建成功、再因列宽写不进元数据而稳定泄漏。
	if _, err := service.Get(ctx, &adksession.GetRequest{
		AppName: "app", UserID: longUser, SessionID: "conversation-1",
	}); err == nil {
		t.Fatal("adk session was created for an oversized user id")
	}
	if _, err := service.Get(ctx, &adksession.GetRequest{
		AppName: "app", UserID: "user-1", SessionID: longConversation,
	}); err == nil {
		t.Fatal("adk session was created for an oversized conversation id")
	}
}

func TestDeleteHidesConversationEvenWhenSessionDeleteFails(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	service := &failingSessionService{Service: adksession.InMemoryService()}
	manager := &Session{name: "app", convRepo: repo, service: service}

	if _, err := manager.Create(ctx, "user-1", "conversation-1"); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	service.deleteErr = errors.New("delete failed")
	// 元数据已删除，对调用方而言删除已完成；ADK 侧的残留不应冒泡成错误。
	if err := manager.Delete(ctx, "user-1", "conversation-1"); err != nil {
		t.Fatalf("Delete() error = %v, want nil", err)
	}
	if _, err := manager.GetMeta(ctx, "user-1", "conversation-1"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("GetMeta() after delete error = %v, want ErrConversationNotFound", err)
	}
	if err := manager.Delete(ctx, "user-1", "conversation-1"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("Delete() repeat error = %v, want ErrConversationNotFound", err)
	}
}

func TestNewInitializesConversationMetadata(t *testing.T) {
	// 临时文件避免命名共享缓存库在 -count>1 时的状态残留
	dialector := sqlite.Open("file:" + filepath.Join(t.TempDir(), "new.db") + "?_busy_timeout=5000&_journal_mode=WAL")
	manager, err := New("app", dialector)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := manager.Create(context.Background(), "user-1", "conversation-1"); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := manager.GetMeta(context.Background(), "user-1", "conversation-1"); err != nil {
		t.Fatalf("GetMeta() error = %v", err)
	}
}

func TestNewRejectsOversizedAppName(t *testing.T) {
	longName := strings.Repeat("a", maxAppNameRunes+1)
	if _, err := New(longName, sqlite.Open(":memory:")); !errors.Is(err, ErrInvalidAppName) {
		t.Fatalf("New(long name) error = %v, want ErrInvalidAppName", err)
	}
	if _, err := New("", sqlite.Open(":memory:")); !errors.Is(err, ErrInvalidAppName) {
		t.Fatalf("New(empty name) error = %v, want ErrInvalidAppName", err)
	}
}

func TestNewSharesBareSQLiteMemoryConnection(t *testing.T) {
	manager, err := New("app", sqlite.Open(":memory:"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()
	if _, err := manager.Create(ctx, "user-1", "conversation-1"); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := manager.GetMeta(ctx, "user-1", "conversation-1"); err != nil {
		t.Fatalf("GetMeta() error = %v", err)
	}
	if _, err := manager.Get(ctx, "user-1", "conversation-1"); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
}

func conversationIDs(conversations []Conversation) []string {
	ids := make([]string, 0, len(conversations))
	for _, conversation := range conversations {
		ids = append(ids, conversation.ConversationID)
	}
	return ids
}
