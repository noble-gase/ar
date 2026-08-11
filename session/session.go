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
	convRepo ConversationRepository
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

// GetOrCreate 返回 userId 当天的自动会话，适用于钉钉等不管理会话 ID 的渠道。
// 会话 ID 由 (应用, 用户, 自然日) 确定性派生，并发调用者和崩溃后的重试天然收敛到
// 同一个会话：ADK 主键保证创建至多成功一次，落败者复查一次即可复用。
// eventNum 限制返回会话携带的最近事件条数，0 表示全量加载；只要会话 ID 的调用方传 1。
func (s *Session) GetOrCreate(ctx context.Context, userId string, eventNum int) (session.Session, error) {
	if len(userId) == 0 || utf8.RuneCountInString(userId) > maxUserIDRunes {
		return nil, ErrInvalidUserID
	}
	conversationId := autoConversationID(s.name, userId)

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

// ResetAutomatic deletes today's deterministic automatic session. The next
// GetOrCreate starts with a clean workflow state and conversation history.
// Automatic sessions have no conversation metadata, so they must be deleted
// directly from the ADK service rather than through Delete.
func (s *Session) ResetAutomatic(ctx context.Context, userId string) error {
	if len(userId) == 0 || utf8.RuneCountInString(userId) > maxUserIDRunes {
		return ErrInvalidUserID
	}
	return s.service.Delete(ctx, &session.DeleteRequest{
		AppName:   s.name,
		UserID:    userId,
		SessionID: autoConversationID(s.name, userId),
	})
}

// autoConversationID 由应用、用户和本地时区的自然日派生，跨自然日自动轮换到新会话。
func autoConversationID(appName, userId string) string {
	today := time.Now().In(time.Local).Format(time.DateOnly)
	name := fmt.Sprintf("argon:auto:%s:%s:%s", appName, userId, today)
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String()
}

// gormConfig 使用 Warn 级别，避免生产环境逐条打印 SQL。
func gormConfig() *gorm.Config {
	return &gorm.Config{
		TranslateError: true,
		Logger: logger.NewSlogLogger(slog.Default(), logger.Config{
			SlowThreshold:             time.Second,
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: true,
		}),
	}
}

// New 初始化 ADK 会话存储和会话元数据存储。连接生命周期由 dialector 的持有方负责。
func New(name string, dialector gorm.Dialector) (*Session, error) {
	if len(name) == 0 || utf8.RuneCountInString(name) > maxAppNameRunes {
		return nil, ErrInvalidAppName
	}

	svc, err := database.NewSessionService(dialector, gormConfig())
	if err != nil {
		return nil, err
	}
	if err = database.AutoMigrate(svc); err != nil {
		return nil, err
	}

	convRepo, err := NewConversationRepository(dialector, gormConfig())
	if err != nil {
		return nil, err
	}
	return &Session{name: name, service: svc, convRepo: convRepo}, nil
}
