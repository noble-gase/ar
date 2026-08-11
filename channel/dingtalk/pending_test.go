package dingtalk

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestPendingConfirmRedisLifecycle(t *testing.T) {
	mr := miniredis.RunT(t)
	store := &CardSender{
		clientId: "test-client",
		reduc:    redis.NewClient(&redis.Options{Addr: mr.Addr()}),
	}
	ctx := context.Background()

	first := &pendingConfirm{CallId: "call-1", UserId: "u1", SessionId: "session-1"}
	second := &pendingConfirm{CallId: "call-2", UserId: "u1", SessionId: "session-1"}
	if err := store.savePending(ctx, "track-1", first); err != nil {
		t.Fatalf("savePending(first) error = %v", err)
	}
	if err := store.savePending(ctx, "track-2", second); err != nil {
		t.Fatalf("savePending(second) error = %v", err)
	}

	got, err := store.loadPending(ctx, "track-1")
	if err != nil {
		t.Fatalf("loadPending() error = %v", err)
	}
	if got.CallId != first.CallId || got.SessionId != first.SessionId {
		t.Fatalf("loadPending() = %+v, want %+v", got, first)
	}
	// 记录必须活得比自动会话久：先于会话过期的话，ADK 里的确认还 pending，
	// 原卡却已经点不动，而 /cancel 只处理图工作流的待答问题、盖不到工具确认。
	if ttl := mr.TTL(store.pendingKey("track-1")); ttl <= 24*time.Hour {
		t.Errorf("pending TTL = %v, want it to outlive the daily conversation rollover", ttl)
	}

	if err := store.dropPending(ctx, "track-1", "u1"); err != nil {
		t.Fatalf("dropPending() error = %v", err)
	}
	if _, err := store.loadPending(ctx, "track-1"); !errors.Is(err, redis.Nil) {
		t.Errorf("load dropped pending error = %v, want redis.Nil", err)
	}

	trackIds, err := store.clearConfirms(ctx, "u1")
	if err != nil {
		t.Fatalf("clearConfirms() error = %v", err)
	}
	if len(trackIds) != 1 || trackIds[0] != "track-2" {
		t.Errorf("clearConfirms() trackIds = %v, want [track-2]", trackIds)
	}
	if _, err := store.loadPending(ctx, "track-2"); !errors.Is(err, redis.Nil) {
		t.Errorf("load cleared pending error = %v, want redis.Nil", err)
	}
}
