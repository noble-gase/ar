package userlock

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestLocker(t *testing.T) *Locker {
	t.Helper()

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { client.Close() })

	return New(client, Config{
		Prefix: "test:lock",
		TTL:    300 * time.Millisecond,
		Renew:  50 * time.Millisecond,
		Retry:  10 * time.Millisecond,
	})
}

// 锁被别人接手后，持锁方的 context 必须被取消，否则会和新持锁者并发处理同一个 key。
func TestLockSignalsLostOwnership(t *testing.T) {
	l := newTestLocker(t)
	ctx := context.Background()

	held, release, err := l.Lock(ctx, "u1")
	if err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	defer release()

	if held.Err() != nil {
		t.Fatalf("context cancelled while the lock was held: %v", held.Err())
	}

	// 模拟锁过期后被另一个实例抢走
	if err := l.client.Set(ctx, l.key("u1"), "someone-else", l.ttl).Err(); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	select {
	case <-held.Done():
	case <-time.After(l.renew + 2*time.Second):
		t.Fatal("context was not cancelled after the lock was taken over")
	}
}

// 正常持锁期间 context 不应被取消。
func TestLockContextStaysAliveWhileHeld(t *testing.T) {
	l := newTestLocker(t)

	held, release, err := l.Lock(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	defer release()

	select {
	case <-held.Done():
		t.Fatalf("context cancelled while the lock was still held: %v", held.Err())
	case <-time.After(l.renew + 500*time.Millisecond):
	}
}

// 跨实例互斥：两个 Locker 共享同一个 Redis，模拟两个服务实例。
func TestLockIsExclusiveAcrossInstances(t *testing.T) {
	shared := newTestLocker(t)
	other := New(shared.client, Config{
		Prefix: shared.prefix,
		TTL:    shared.ttl,
		Renew:  shared.renew,
		Retry:  shared.retry,
	})

	ctx := context.Background()

	_, release, err := shared.Lock(ctx, "u1")
	if err != nil {
		t.Fatalf("Lock() error = %v", err)
	}

	// 另一个实例必须拿不到，直到第一个释放
	locked := make(chan struct{})
	go func() {
		_, unlock, err := other.Lock(ctx, "u1")
		if err != nil {
			t.Errorf("Lock() on second instance error = %v", err)
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

// 不同 key 互不阻塞。
func TestLockIsPerKey(t *testing.T) {
	l := newTestLocker(t)
	ctx := context.Background()

	_, release1, err := l.Lock(ctx, "u1")
	if err != nil {
		t.Fatalf("Lock(u1) error = %v", err)
	}
	defer release1()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, release2, err := l.Lock(ctx, "u2")
		if err != nil {
			t.Errorf("Lock(u2) error = %v", err)
			return
		}
		release2()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a different key was blocked")
	}
}

// 释放只能删掉自己持有的锁：锁过期被他人接手后，原持有者的释放不能误删。
func TestLockReleaseDoesNotDropOthers(t *testing.T) {
	l := newTestLocker(t)
	ctx := context.Background()

	_, release, err := l.Lock(ctx, "u1")
	if err != nil {
		t.Fatalf("Lock() error = %v", err)
	}

	// 模拟锁过期后被另一个实例接手
	key := l.key("u1")
	if err := l.client.Set(ctx, key, "someone-else", l.ttl).Err(); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	release()

	got, err := l.client.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got != "someone-else" {
		t.Errorf("lock value = %q, want the new holder's lock left intact", got)
	}
}

// 抢锁等待有独立上限：排队方应尽早得到「正忙」的答复，而不是陪着持有方
// 跑完整个业务超时。
func TestLockWaitTimesOutAsBusy(t *testing.T) {
	l := newTestLocker(t)
	l.wait = 150 * time.Millisecond

	_, release, err := l.Lock(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	defer release()

	start := time.Now()
	if _, _, err := l.Lock(context.Background(), "u1"); !errors.Is(err, ErrBusy) {
		t.Fatalf("Lock() error = %v, want ErrBusy", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Lock() waited %v before reporting busy", elapsed)
	}
}

// ctx 取消时不能一直阻塞。
func TestLockRespectsContext(t *testing.T) {
	l := newTestLocker(t)

	_, release, err := l.Lock(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, _, err := l.Lock(ctx, "u1"); err == nil {
		t.Fatal("Lock() error = nil, want a context error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Lock() blocked for %v after the context expired", elapsed)
	}
}

// 持锁期间要持续续期，长任务不能因为锁提前过期被别人插队。
func TestLockIsRenewedWhileHeld(t *testing.T) {
	l := newTestLocker(t)
	ctx := context.Background()

	_, release, err := l.Lock(ctx, "u1")
	if err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	defer release()

	key := l.key("u1")
	ttl, err := l.client.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("PTTL() error = %v", err)
	}
	if ttl <= 0 || ttl > l.ttl {
		t.Fatalf("PTTL = %v, want (0, %v]", ttl, l.ttl)
	}
	if l.renew >= l.ttl {
		t.Fatalf("renew interval %v must be shorter than the TTL %v", l.renew, l.ttl)
	}
}

// 同一实例内的并发抢锁也必须互斥。
func TestLockSerializesConcurrentCallers(t *testing.T) {
	l := newTestLocker(t)
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

			_, unlock, err := l.Lock(ctx, "u1")
			if err != nil {
				t.Errorf("Lock() error = %v", err)
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

// 零值配置全部落到默认值，Prefix 隔离生效。
func TestNewAppliesDefaults(t *testing.T) {
	l := New(nil, Config{})
	if l.ttl != defaultTTL || l.renew != defaultRenew || l.retry != defaultRetry || l.wait != defaultWait {
		t.Errorf("defaults not applied: %+v", l)
	}
	if l.key("u1") != "argon:userlock:u1" {
		t.Errorf("key = %q, want the default prefix applied", l.key("u1"))
	}
}

// Renew >= TTL 会让锁周期性过期、被别人插队——恰是本包要防的事，
// 不能被一行配置绕过，New 必须钳住这个不变量。
func TestNewClampsRenewBelowTTL(t *testing.T) {
	l := New(nil, Config{TTL: 300 * time.Millisecond, Renew: time.Second})
	if l.renew >= l.ttl {
		t.Fatalf("renew = %v with ttl = %v, want renew clamped below ttl", l.renew, l.ttl)
	}
	if l.renew != l.ttl/3 {
		t.Errorf("renew = %v, want ttl/3 = %v", l.renew, l.ttl/3)
	}
}
