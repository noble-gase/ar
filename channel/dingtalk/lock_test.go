package dingtalk

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestCardSender(t *testing.T) *CardSender {
	t.Helper()

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { client.Close() })

	return &CardSender{
		clientId: "test-client",
		reduc:    client,

		lockTTLOverride:   300 * time.Millisecond,
		lockRenewOverride: 50 * time.Millisecond,
		lockRetryOverride: 10 * time.Millisecond,
	}
}

// 锁被别人接手后，持锁方的 context 必须被取消，否则会和新持锁者并发写会话。
func TestUserLockSignalsLostOwnership(t *testing.T) {
	s := newTestCardSender(t)
	ctx := context.Background()

	held, release, err := s.lockUser(ctx, "u1")
	if err != nil {
		t.Fatalf("lockUser() error = %v", err)
	}
	defer release()

	if held.Err() != nil {
		t.Fatalf("context cancelled while the lock was held: %v", held.Err())
	}

	// 模拟锁过期后被另一个实例抢走
	if err := s.reduc.Set(ctx, s.userLockKey("u1"), "someone-else", s.lockTTL()).Err(); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	select {
	case <-held.Done():
	case <-time.After(s.lockRenew() + 2*time.Second):
		t.Fatal("context was not cancelled after the lock was taken over")
	}
}

// 正常持锁期间 context 不应被取消。
func TestUserLockContextStaysAliveWhileHeld(t *testing.T) {
	s := newTestCardSender(t)

	held, release, err := s.lockUser(context.Background(), "u1")
	if err != nil {
		t.Fatalf("lockUser() error = %v", err)
	}
	defer release()

	select {
	case <-held.Done():
		t.Fatalf("context cancelled while the lock was still held: %v", held.Err())
	case <-time.After(s.lockRenew() + 500*time.Millisecond):
	}
}

// 跨实例互斥：两个 CardSender 共享同一个 Redis，模拟两个 bot 实例。
func TestUserLockIsExclusiveAcrossInstances(t *testing.T) {
	shared := newTestCardSender(t)
	other := &CardSender{
		clientId:          shared.clientId,
		reduc:             shared.reduc,
		lockTTLOverride:   shared.lockTTLOverride,
		lockRenewOverride: shared.lockRenewOverride,
		lockRetryOverride: shared.lockRetryOverride,
	}

	ctx := context.Background()

	_, release, err := shared.lockUser(ctx, "u1")
	if err != nil {
		t.Fatalf("lockUser() error = %v", err)
	}

	// 另一个实例必须拿不到，直到第一个释放
	locked := make(chan struct{})
	go func() {
		_, unlock, err := other.lockUser(ctx, "u1")
		if err != nil {
			t.Errorf("lockUser() on second instance error = %v", err)
			close(locked)
			return
		}
		close(locked)
		unlock()
	}()

	select {
	case <-locked:
		t.Fatal("second instance acquired the lock while it was held")
	case <-time.After(300 * time.Millisecond):
	}

	release()

	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		t.Fatal("second instance never acquired the lock after release")
	}
}

// 不同用户互不阻塞。
func TestUserLockIsPerUser(t *testing.T) {
	s := newTestCardSender(t)
	ctx := context.Background()

	_, release1, err := s.lockUser(ctx, "u1")
	if err != nil {
		t.Fatalf("lockUser(u1) error = %v", err)
	}
	defer release1()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, release2, err := s.lockUser(ctx, "u2")
		if err != nil {
			t.Errorf("lockUser(u2) error = %v", err)
			return
		}
		release2()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a different user was blocked")
	}
}

// 释放只能删掉自己持有的锁：锁过期被他人接手后，原持有者的释放不能误删。
func TestUserLockReleaseDoesNotDropOthers(t *testing.T) {
	s := newTestCardSender(t)
	ctx := context.Background()

	_, release, err := s.lockUser(ctx, "u1")
	if err != nil {
		t.Fatalf("lockUser() error = %v", err)
	}

	// 模拟锁过期后被另一个实例接手
	key := s.userLockKey("u1")
	if err := s.reduc.Set(ctx, key, "someone-else", s.lockTTL()).Err(); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	release()

	got, err := s.reduc.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got != "someone-else" {
		t.Errorf("lock value = %q, want the new holder's lock left intact", got)
	}
}

// ctx 取消时不能一直阻塞。
func TestUserLockRespectsContext(t *testing.T) {
	s := newTestCardSender(t)

	_, release, err := s.lockUser(context.Background(), "u1")
	if err != nil {
		t.Fatalf("lockUser() error = %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, _, err := s.lockUser(ctx, "u1"); err == nil {
		t.Fatal("lockUser() error = nil, want a context error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("lockUser() blocked for %v after the context expired", elapsed)
	}
}

// 持锁期间要持续续期，长任务不能因为锁提前过期被别人插队。
func TestUserLockIsRenewedWhileHeld(t *testing.T) {
	s := newTestCardSender(t)
	ctx := context.Background()

	_, release, err := s.lockUser(ctx, "u1")
	if err != nil {
		t.Fatalf("lockUser() error = %v", err)
	}
	defer release()

	key := s.userLockKey("u1")
	ttl, err := s.reduc.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("PTTL() error = %v", err)
	}
	if ttl <= 0 || ttl > s.lockTTL() {
		t.Fatalf("PTTL = %v, want (0, %v]", ttl, s.lockTTL())
	}
	if s.lockRenew() >= s.lockTTL() {
		t.Fatalf("renew interval %v must be shorter than the TTL %v", s.lockRenew(), s.lockTTL())
	}
}

// 同一实例内的并发抢锁也必须互斥。
func TestUserLockSerializesConcurrentCallers(t *testing.T) {
	s := newTestCardSender(t)
	ctx := context.Background()

	var (
		inFlight atomic.Int32
		peak     atomic.Int32
		wg       sync.WaitGroup
	)

	const callers = 5
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()

			_, unlock, err := s.lockUser(ctx, "u1")
			if err != nil {
				t.Errorf("lockUser() error = %v", err)
				return
			}
			defer unlock()

			now := inFlight.Add(1)
			for {
				best := peak.Load()
				if now <= best || peak.CompareAndSwap(best, now) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			inFlight.Add(-1)
		}()
	}
	wg.Wait()

	if got := peak.Load(); got != 1 {
		t.Errorf("max concurrent lock holders = %d, want 1", got)
	}
}
