package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/noble-gase/neon/redlock"
	"github.com/redis/go-redis/v9"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Session struct {
	name    string
	service session.Service
	reduc   redis.UniversalClient

	// maxIdle rotates a session when its last update is older than this. When
	// zero, sessions are reused indefinitely (no rotation).
	maxIdle time.Duration
}

// Option configures a Session.
type Option func(*Session)

// WithMaxIdle rotates (creates a fresh) session once the existing one has been
// idle for longer than d, bounding conversation-history growth. Zero disables
// rotation.
func WithMaxIdle(d time.Duration) Option {
	return func(s *Session) { s.maxIdle = d }
}

func (s *Session) AppName() string {
	return s.name
}

func (s *Session) Service() session.Service {
	return s.service
}

func (s *Session) mutexKey(userId string) string {
	return fmt.Sprintf("%s:session-mutex:%s", s.name, userId)
}

func (s *Session) cacheKey(userId string) string {
	return fmt.Sprintf("%s:session:%s", s.name, userId)
}

func (s *Session) GetOrCreate(ctx context.Context, userId string) (string, error) {
	key := s.cacheKey(userId)

	sid, err := s.loadCachedSession(ctx, key)
	if err != nil {
		return "", err
	}
	if len(sid) != 0 {
		return sid, nil
	}

	lock := redlock.New(s.reduc, s.mutexKey(userId), 10*time.Minute)
	if err = lock.TryAcquire(ctx, 60, time.Second); err != nil {
		return "", err
	}
	defer lock.Release(ctx)

	sid, err = s.loadCachedSession(ctx, key)
	if err != nil {
		return "", err
	}
	if len(sid) != 0 {
		return sid, nil
	}

	// 从数据库中获取
	sid, err = s.fetchSession(ctx, userId)
	if err != nil {
		return "", err
	}
	// 如果没有获取到，创建一个新的
	if len(sid) == 0 {
		resp, _err := s.service.Create(ctx, &session.CreateRequest{
			AppName: s.name,
			UserID:  userId,
		})
		if _err != nil {
			return "", _err
		}
		sid = resp.Session.ID()
	}
	if err = s.reduc.Set(ctx, key, sid, s.maxIdle).Err(); err != nil {
		slog.ErrorContext(ctx, "[session] redis set failed", slog.String("key", key), slog.String("sid", sid))
	}

	return sid, nil
}

func (s *Session) loadCachedSession(ctx context.Context, key string) (string, error) {
	sid, err := s.reduc.Get(ctx, key).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return "", err
	}
	return sid, nil
}

// fetchSession returns the most recently updated session for a user, or an
// empty string when none exists (or the newest has been idle past maxIdle, so a
// fresh session should be created).
func (s *Session) fetchSession(ctx context.Context, userId string) (string, error) {
	list, err := s.service.List(ctx, &session.ListRequest{
		AppName: s.name,
		UserID:  userId,
	})
	if err != nil {
		return "", err
	}
	if len(list.Sessions) == 0 {
		return "", nil
	}

	newest := list.Sessions[0]
	for _, sess := range list.Sessions[1:] {
		if sess.LastUpdateTime().After(newest.LastUpdateTime()) {
			newest = sess
		}
	}

	if s.maxIdle > 0 && time.Since(newest.LastUpdateTime()) > s.maxIdle {
		return "", nil // 已空闲过久，轮转到新会话
	}
	return newest.ID(), nil
}

func New(name string, db gorm.Dialector, uc redis.UniversalClient, opts ...Option) (*Session, error) {
	svc, err := database.NewSessionService(db, &gorm.Config{
		Logger: logger.NewSlogLogger(slog.Default(), logger.Config{
			SlowThreshold:             time.Second,
			LogLevel:                  logger.Info,
			IgnoreRecordNotFoundError: true,
		}),
	})
	if err != nil {
		return nil, err
	}
	if err = database.AutoMigrate(svc); err != nil {
		return nil, err
	}

	s := &Session{
		name:    name,
		service: svc,
		reduc:   uc,
		maxIdle: time.Hour, // 默认一小时轮转
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}
