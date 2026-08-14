package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Session struct {
	name     string
	service  session.Service
	convRepo *conversationRepository
	autoRepo *autoConversationRepository

	// loc 决定自动会话按自然日轮换的边界，见 WithLocation。
	loc *time.Location

	// logLevel 决定 GORM 的日志级别，默认 logger.Warn。
	logLevel logger.LogLevel
}

// today 返回配置时区下的自然日，作为自动会话的轮换边界。
func (s *Session) today() string {
	return time.Now().In(s.loc).Format(time.DateOnly)
}

func (s *Session) AppName() string {
	return s.name
}

func (s *Session) Service() session.Service {
	return s.service
}

// Create 先创建 ADK 会话，再写入元数据。
// 元数据是会话可见性的唯一依据，放在最后一步可保证失败时至多留下一个不可见的
// ADK 会话，而不会出现"列表里有、打开却 404"。conversationId 为空时生成 UUID。
func (s *Session) Create(ctx context.Context, userId, conversationId string) (session.Session, error) {
	if len(userId) == 0 || utf8.RuneCountInString(userId) > maxUserIDRunes {
		return nil, ErrInvalidUserID
	}

	if len(conversationId) == 0 {
		conversationId = uuid.NewString()
	} else if utf8.RuneCountInString(conversationId) > maxConvIDRunes {
		return nil, ErrInvalidConversationID
	}

	resp, err := s.service.Create(ctx, &session.CreateRequest{
		AppName:   s.name,
		UserID:    userId,
		SessionID: conversationId,
	})
	if err != nil {
		return nil, err
	}

	now := time.Now()
	err = s.convRepo.Create(ctx, &Conversation{
		AppName:        s.name,
		UserID:         userId,
		ConversationID: conversationId,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		return nil, err
	}
	return resp.Session, nil
}

// GetMeta 仅在会话属于 userId 时返回其元数据。
func (s *Session) GetMeta(ctx context.Context, userId, conversationId string) (*Conversation, error) {
	return s.convRepo.Get(ctx, s.name, userId, conversationId)
}

// Touch 校验归属关系并将会话移到用户列表前部。
func (s *Session) Touch(ctx context.Context, userId, conversationId string) error {
	return s.convRepo.Touch(ctx, s.name, userId, conversationId, time.Now())
}

// Rename 校验归属关系并改写标题，不影响列表排序。
func (s *Session) Rename(ctx context.Context, userId, conversationId, title string) error {
	if utf8.RuneCountInString(title) > maxTitleRunes {
		return ErrTitleTooLong
	}
	return s.convRepo.Rename(ctx, s.name, userId, conversationId, title)
}

// Get 仅在会话属于 userId 时返回会话及其事件。
func (s *Session) Get(ctx context.Context, userId, conversationId string) (session.Session, error) {
	if _, err := s.GetMeta(ctx, userId, conversationId); err != nil {
		return nil, err
	}

	resp, err := s.service.Get(ctx, &session.GetRequest{
		AppName:   s.name,
		UserID:    userId,
		SessionID: conversationId,
	})
	if err != nil {
		// 元数据在而 ADK 会话不在（例如被外部删除），对调用方就是不存在。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrConversationNotFound
		}
		return nil, err
	}
	return resp.Session, nil
}

func (s *Session) List(ctx context.Context, userId, cursor string, limit int) (*ConversationPage, error) {
	return s.convRepo.List(ctx, s.name, userId, cursor, limit)
}

// Delete 先移除元数据，使会话立即对用户不可见，再删除 ADK 会话。
// 第二步失败只会留下用户无法寻址的孤儿会话，不会让已删除的会话重新出现在列表里。
func (s *Session) Delete(ctx context.Context, userId, conversationId string) error {
	if err := s.convRepo.Delete(ctx, s.name, userId, conversationId); err != nil {
		return err
	}
	if err := s.service.Delete(ctx, &session.DeleteRequest{
		AppName:   s.name,
		UserID:    userId,
		SessionID: conversationId,
	}); err != nil {
		// 元数据已删除，对调用方而言删除已经完成，这里只记录残留供人工清理。
		slog.ErrorContext(ctx, "[session] orphaned adk session after delete",
			slog.String("conversationId", conversationId), slog.Any("error", err))
	}
	return nil
}

// GetOrCreate 返回 userId 当前的自动会话，适用于钉钉等不管理会话 ID 的渠道。
// 会话 ID 来自 (应用, 用户) 的指针记录，跨自然日（按配置时区）自动轮换到新会话。
// 并发调用者在指针表上通过条件写收敛到同一个 ID；ADK 会话的创建竞态由主键约束
// 保证至多成功一次，落败者复查一次即可复用。
// eventNum 限制返回会话携带的最近事件条数，0 表示全量加载；只要会话 ID 的调用方传 1。
func (s *Session) GetOrCreate(ctx context.Context, userId string, eventNum int) (session.Session, error) {
	if len(userId) == 0 || utf8.RuneCountInString(userId) > maxUserIDRunes {
		return nil, ErrInvalidUserID
	}
	conversationId, err := s.autoRepo.Current(ctx, s.name, userId, s.today())
	if err != nil {
		return nil, err
	}

	load := func() session.Session {
		resp, err := s.service.Get(ctx, &session.GetRequest{
			AppName:         s.name,
			UserID:          userId,
			SessionID:       conversationId,
			NumRecentEvents: eventNum,
		})
		if err != nil || resp == nil {
			return nil
		}
		return resp.Session
	}

	if sess := load(); sess != nil {
		return sess, nil
	}
	resp, err := s.service.Create(ctx, &session.CreateRequest{
		AppName:   s.name,
		UserID:    userId,
		SessionID: conversationId,
	})
	if err != nil {
		// 并发者已创建或提交后响应丢失时，按确定性 ID 仍能读到会话。
		if sess := load(); sess != nil {
			return sess, nil
		}
		return nil, err
	}
	return resp.Session, nil
}

// ResetAutomatic 把自动会话指针轮换到一个全新的 ID，并尽力删除旧会话。当前会话
// 连同其中暂停的图工作流随之不可达，下一次 GetOrCreate 会以干净的状态重新开始。
//
// 先换指针再删除：指针一换，旧会话就对渠道不可达了，删除失败至多留下一个无法
// 寻址的孤儿（与 Delete 的孤儿语义一致），不影响正确性。读指针和换指针之间没有
// 原子性，但渠道侧的重置都在用户锁内串行化；即便真的竞争，最坏也只是漏删一个
// 孤儿。指向旧会话的确认卡片会因会话不匹配而得到 ErrConversationChanged，与
// 跨日轮换走同一条失效路径。
func (s *Session) ResetAutomatic(ctx context.Context, userId string) error {
	if len(userId) == 0 || utf8.RuneCountInString(userId) > maxUserIDRunes {
		return ErrInvalidUserID
	}

	old, err := s.autoRepo.CurrentID(ctx, s.name, userId)
	if err != nil {
		return err
	}
	if err := s.autoRepo.Rotate(ctx, s.name, userId, s.today()); err != nil {
		return err
	}

	if old != "" {
		if err := s.service.Delete(ctx, &session.DeleteRequest{
			AppName:   s.name,
			UserID:    userId,
			SessionID: old,
		}); err != nil {
			slog.ErrorContext(ctx, "[session] delete rotated automatic session failed",
				slog.String("conversationId", old), slog.Any("error", err))
		}
	}
	return nil
}

func gormConfig(logLevel logger.LogLevel) *gorm.Config {
	return &gorm.Config{
		TranslateError: true,
		Logger: logger.NewSlogLogger(slog.Default(), logger.Config{
			SlowThreshold:             time.Second,
			LogLevel:                  logLevel,
			IgnoreRecordNotFoundError: true,
		}),
	}
}

// New 初始化 ADK 会话存储、会话元数据存储和自动会话指针存储。
//
// 元数据与指针共用一个连接池；ADK service 的池由它自己持有
// （database.NewSessionService 只接受 dialector，无法注入现成连接）。
func New(name string, dialector gorm.Dialector, opts ...Option) (*Session, error) {
	if len(name) == 0 || utf8.RuneCountInString(name) > maxAppNameRunes {
		return nil, ErrInvalidAppName
	}

	s := &Session{
		name:     name,
		loc:      time.Local,
		logLevel: logger.Warn,
	}
	for _, opt := range opts {
		opt(s)
	}

	svc, err := database.NewSessionService(dialector, gormConfig(s.logLevel))
	if err != nil {
		return nil, err
	}
	if err = database.AutoMigrate(svc); err != nil {
		return nil, err
	}

	db, err := gorm.Open(dialector, gormConfig(s.logLevel))
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&Conversation{}, &autoConversation{}); err != nil {
		return nil, fmt.Errorf("migrate session metadata: %w", err)
	}

	s.service = svc
	s.convRepo = &conversationRepository{db: db}
	s.autoRepo = &autoConversationRepository{db: db}

	return s, nil
}
