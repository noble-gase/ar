package session

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"google.golang.org/genai"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newTestManager(t *testing.T) *Session {
	t.Helper()

	// 用临时文件 + WAL 而不是共享缓存内存库：并发写共享缓存库会报
	// SQLITE_LOCKED（table is locked），它不受 busy_timeout 约束，
	// 会让并发收敛类测试无谓地失败。
	dsn := "file:" + filepath.Join(t.TempDir(), "session.db") + "?_busy_timeout=5000&_journal_mode=WAL"
	db, err := gorm.Open(sqlite.Open(dsn), gormConfig())
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.AutoMigrate(&Conversation{}, &autoConversation{}); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	return &Session{
		name:     "test",
		service:  adksession.InMemoryService(),
		convRepo: &conversationRepository{db: db},
		autoRepo: &autoConversationRepository{db: db},
		loc:      time.Local,
	}
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

// ThoughtSignature 必须在会话持久化中原样存活：Anthropic 扩展思考的往返依赖
// 签名随事件存进 DB、下一轮取出还原成 thinking 块。签名在存储层被丢掉的话，
// 带工具调用的第二轮请求会被 API 拒绝，且现象与完全不支持思考时一模一样，
// 所以这里用真实的 database service（而不是 InMemory）覆盖 JSON 序列化往返。
func TestThoughtSignatureSurvivesPersistence(t *testing.T) {
	ctx := context.Background()

	dsn := "file:" + filepath.Join(t.TempDir(), "thought.db") + "?_busy_timeout=5000&_journal_mode=WAL"
	service, err := database.NewSessionService(sqlite.Open(dsn), gormConfig())
	if err != nil {
		t.Fatalf("NewSessionService() error = %v", err)
	}
	if err := database.AutoMigrate(service); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}

	created, err := service.Create(ctx, &adksession.CreateRequest{
		AppName: "app", UserID: "user-1", SessionID: "conversation-1",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	event := &adksession.Event{Author: "model", InvocationID: "invocation-1"}
	event.Content = &genai.Content{
		Role: "model",
		Parts: []*genai.Part{
			{Text: "let me think", Thought: true, ThoughtSignature: []byte("sig-bytes")},
			{Text: "the answer"},
		},
	}
	if err := service.AppendEvent(ctx, created.Session, event); err != nil {
		t.Fatalf("AppendEvent() error = %v", err)
	}

	got, err := service.Get(ctx, &adksession.GetRequest{
		AppName: "app", UserID: "user-1", SessionID: "conversation-1",
	})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	events := got.Session.Events()
	if events.Len() != 1 {
		t.Fatalf("events.Len() = %d, want 1", events.Len())
	}
	parts := events.At(0).Content.Parts
	if len(parts) != 2 {
		t.Fatalf("len(parts) = %d, want 2", len(parts))
	}
	if !parts[0].Thought || parts[0].Text != "let me think" {
		t.Errorf("parts[0] = %+v, want the thought part intact", parts[0])
	}
	if string(parts[0].ThoughtSignature) != "sig-bytes" {
		t.Errorf("ThoughtSignature = %q, want the signature to survive persistence", parts[0].ThoughtSignature)
	}
	if parts[1].Thought || parts[1].Text != "the answer" {
		t.Errorf("parts[1] = %+v, want the plain text part", parts[1])
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

// 重置必须轮换到全新的会话 ID：旧卡片凭 ID 不匹配即可判定失效，不存在
// 「同日重置后 ID 复用」的歧义；旧会话被尽力删除，不留无法寻址的孤儿。
func TestResetAutomaticRotatesToFreshSession(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t)

	first, err := manager.GetOrCreate(ctx, "user-1", 1)
	if err != nil {
		t.Fatalf("GetOrCreate() error = %v", err)
	}
	if err := first.State().Set("waiting", true); err != nil {
		t.Fatalf("State().Set() error = %v", err)
	}

	if err := manager.ResetAutomatic(ctx, "user-1"); err != nil {
		t.Fatalf("ResetAutomatic() error = %v", err)
	}
	second, err := manager.GetOrCreate(ctx, "user-1", 1)
	if err != nil {
		t.Fatalf("GetOrCreate() after reset error = %v", err)
	}
	if second.ID() == first.ID() {
		t.Fatalf("recreated ID = %q, want a fresh conversation id", second.ID())
	}
	if _, err := second.State().Get("waiting"); err == nil {
		t.Fatal("fresh automatic session retained old workflow state")
	}

	// 旧会话随重置一起清理，不作为孤儿残留
	if _, err := manager.service.Get(ctx, &adksession.GetRequest{
		AppName: "test", UserID: "user-1", SessionID: first.ID(),
	}); err == nil {
		t.Fatal("old session should be deleted on reset")
	}
}

// 删除旧会话失败只记日志：指针已经换掉，重置对调用方就是成功的，
// 最坏只留下一个无法寻址的孤儿。
func TestResetAutomaticSucceedsWhenOldSessionDeleteFails(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t)
	service := &failingSessionService{Service: manager.service}
	manager.service = service

	first, err := manager.GetOrCreate(ctx, "user-1", 1)
	if err != nil {
		t.Fatalf("GetOrCreate() error = %v", err)
	}

	service.deleteErr = errors.New("delete failed")
	if err := manager.ResetAutomatic(ctx, "user-1"); err != nil {
		t.Fatalf("ResetAutomatic() error = %v, want nil despite cleanup failure", err)
	}

	second, err := manager.GetOrCreate(ctx, "user-1", 1)
	if err != nil {
		t.Fatalf("GetOrCreate() after reset error = %v", err)
	}
	if second.ID() == first.ID() {
		t.Fatal("pointer must rotate even when the old session cleanup fails")
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

	// 指针表的条件写保证并发调用者收敛到同一个会话
	want := ""
	for range callers {
		if err := <-errs; err != nil {
			t.Fatalf("GetOrCreate() error = %v", err)
		}
		id := <-ids
		if id == "" {
			t.Fatal("GetOrCreate() returned an empty id")
		}
		if want == "" {
			want = id
			continue
		}
		if id != want {
			t.Fatalf("concurrent id = %q, want %q", id, want)
		}
	}
}

// 指针的轮换规则：同日复用、跨日换新、换新后稳定。直接对指针存储测试，
// 用显式的日期入参避免和真实时钟耦合。
func TestAutoConversationPointerRotation(t *testing.T) {
	ctx := context.Background()
	repo := newTestManager(t).autoRepo

	day1, err := repo.Current(ctx, "test", "u1", "2026-08-11")
	if err != nil {
		t.Fatalf("Current(day1) error = %v", err)
	}
	same, err := repo.Current(ctx, "test", "u1", "2026-08-11")
	if err != nil {
		t.Fatalf("Current(day1 again) error = %v", err)
	}
	if same != day1 {
		t.Fatalf("same-day id = %q, want %q reused", same, day1)
	}

	day2, err := repo.Current(ctx, "test", "u1", "2026-08-12")
	if err != nil {
		t.Fatalf("Current(day2) error = %v", err)
	}
	if day2 == day1 {
		t.Fatal("crossing the day boundary must rotate to a fresh id")
	}
	stable, err := repo.Current(ctx, "test", "u1", "2026-08-12")
	if err != nil {
		t.Fatalf("Current(day2 again) error = %v", err)
	}
	if stable != day2 {
		t.Fatalf("post-rotation id = %q, want %q reused", stable, day2)
	}

	// 轮换是单调的：日期靠后的指针不会被日期靠前的调用方（时区配错、时钟回拨）
	// 推回去，否则多实例会用新会话互相踩踏
	backward, err := repo.Current(ctx, "test", "u1", "2026-08-11")
	if err != nil {
		t.Fatalf("Current(earlier day) error = %v", err)
	}
	if backward != day2 {
		t.Fatalf("backward id = %q, want %q kept (rotation must be monotonic)", backward, day2)
	}

	if err := repo.Rotate(ctx, "test", "u1", "2026-08-12"); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	rotated, err := repo.Current(ctx, "test", "u1", "2026-08-12")
	if err != nil {
		t.Fatalf("Current(after rotate) error = %v", err)
	}
	if rotated == day2 {
		t.Fatal("Rotate() must yield a fresh id even within the same day")
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
	// 临时文件而不是命名的共享缓存内存库：后者在同进程内跨运行（-count>1）共享
	// 状态，种下的会话会残留到下一轮
	dialector := sqlite.Open("file:" + filepath.Join(t.TempDir(), "conflict.db") + "?_busy_timeout=5000&_journal_mode=WAL")
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
