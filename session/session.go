package session

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sunmi-OS/gocore/v2/glog"
	"gomod.sunmi.com/gomoddepend/golib/helper"
	"gomod.sunmi.com/gomoddepend/golib/redkit"
	"google.golang.org/adk/session"
	"google.golang.org/adk/session/database"
	"gorm.io/gorm"
)

type Session struct {
	name    string
	service session.Service
	reduc   redis.UniversalClient
}

func (s *Session) AppName() string {
	return s.name
}

func (s *Session) Service() session.Service {
	return s.service
}

func (s *Session) GetOrCreate(ctx context.Context, userId string) (string, error) {
	key := fmt.Sprintf("%s:session:%s", s.name, userId)

	sid, err := redkit.Get(ctx, s.reduc, key, func(ctx context.Context) (string, error) {
		// 从数据库中获取
		sid, err := s.fetchSession(ctx, userId)
		if err != nil {
			return "", err
		}
		if len(sid) != 0 {
			return sid, nil
		}

		// 创建新的会话
		sid, err = s.createSession(ctx, userId)
		if err != nil {
			return "", err
		}
		return sid, nil
	}, 24*time.Hour)
	if err != nil {
		return "", err
	}
	return sid, nil
}

func (s *Session) fetchSession(ctx context.Context, userId string) (string, error) {
	list, err := s.service.List(ctx, &session.ListRequest{
		AppName: s.name,
		UserID:  userId,
	})
	if err != nil {
		return "", err
	}
	if len(list.Sessions) != 0 {
		return list.Sessions[0].ID(), nil
	}
	return "", nil
}

func (s *Session) createSession(ctx context.Context, userId string) (string, error) {
	resp, err := s.service.Create(ctx, &session.CreateRequest{
		AppName: s.name,
		UserID:  userId,
	})
	if err != nil {
		// 多实例竞争：另一个实例抢先创建，重试 List
		if helper.IsUniqueDuplicateError(err) {
			return s.fetchSession(ctx, userId)
		}
		return "", err
	}
	return resp.Session.ID(), nil
}

func New(name string, db gorm.Dialector, uc redis.UniversalClient) (*Session, error) {
	svc, err := database.NewSessionService(db, &gorm.Config{
		Logger: glog.NewDBLogger(true, time.Second),
	})
	if err != nil {
		return nil, err
	}
	if err = database.AutoMigrate(svc); err != nil {
		return nil, err
	}

	return &Session{
		name:    name,
		service: svc,
		reduc:   uc,
	}, nil
}
