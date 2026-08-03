package session

import (
	"time"
)

// Option configures a Session.
type Option func(*Session)

// WithAutoMode enables automatic conversations (for channels such as DingTalk
// that do not manage conversation IDs). Without it only explicit conversations
// are available and GetOrCreate returns ErrAutoModeUnavailable. Automatic
// conversations are tracked in the conversation metadata store; no redis is
// required.
func WithAutoMode() Option {
	return func(s *Session) { s.autoMode = true }
}

// WithReconcileInterval configures periodic recovery of interrupted explicit
// conversation operations. A non-positive duration disables the worker.
func WithReconcileInterval(d time.Duration) Option {
	return func(s *Session) { s.reconcileEvery = d }
}
