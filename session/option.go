package session

import (
	"time"

	"gorm.io/gorm/logger"
)

// Option 配置 Session 的可选行为。
type Option func(*Session)

// WithLogLevel 设置 GORM 的日志级别，默认 logger.Warn。
func WithLogLevel(logLevel logger.LogLevel) Option {
	return func(s *Session) {
		s.logLevel = logLevel
	}
}

// WithLocation 设置自动会话按自然日轮换所用的时区，默认 time.Local。
// 多实例部署必须显式配置同一时区，否则各实例的「当天」边界不一致，
// 同一用户会被路由到不同的自动会话。
func WithLocation(loc *time.Location) Option {
	return func(s *Session) {
		if loc != nil {
			s.loc = loc
		}
	}
}
