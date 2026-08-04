package session

import (
	"time"
)

// Option 用于配置 Session。
type Option func(*Session)

// WithAutoMode 启用自动会话模式，适用于钉钉等不管理会话 ID 的渠道。
// 未启用时仅支持显式会话，GetOrCreate 会返回 ErrAutoModeUnavailable。
// 自动会话由会话元数据存储跟踪，不依赖 Redis。
func WithAutoMode() Option {
	return func(s *Session) { s.autoMode = true }
}

// WithReconcileInterval 配置显式会话中断操作的周期恢复间隔。
// 非正数会禁用后台协调任务。
func WithReconcileInterval(d time.Duration) Option {
	return func(s *Session) { s.reconcileEvery = d }
}
