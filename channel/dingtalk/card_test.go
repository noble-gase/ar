package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/noble-gase/argon/userlock"
	"github.com/redis/go-redis/v9"
)

func newTestCardSender(t *testing.T) *CardSender {
	t.Helper()

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { client.Close() })

	s := &CardSender{
		clientId: "test-client",
		reduc:    client,
		lock:     userlock.New(client, userlock.Config{Prefix: "test:lock"}),
	}
	// 与 NewCardSender 一致的默认；token 相关测试会按需覆盖
	s.fetchToken = s.fetchAccessToken
	return s
}

// token 的获取顺序：进程内缓存 → Redis → 直连刷新。前两级不发任何 HTTP 请求，
// 直连刷新需要真实凭据，由 NewCardSender 启动时的初始化校验覆盖。
func TestAccessTokenUsesRedisThenMemory(t *testing.T) {
	s := newTestCardSender(t)
	s.tokenKey = "adk:access_token:test"
	ctx := context.Background()

	at := AccessToken{Token: "cached-token", ExpiredAt: time.Now().Unix() + 3600}
	b, err := json.Marshal(at)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := s.reduc.Set(ctx, s.tokenKey, string(b), time.Hour).Err(); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	got, err := s.accessToken(ctx)
	if err != nil {
		t.Fatalf("accessToken() error = %v", err)
	}
	if got != "cached-token" {
		t.Fatalf("accessToken() = %q, want the token from redis", got)
	}

	// 第二次命中进程内缓存：删掉 Redis 键也不受影响
	if err := s.reduc.Del(ctx, s.tokenKey).Err(); err != nil {
		t.Fatalf("Del() error = %v", err)
	}
	got, err = s.accessToken(ctx)
	if err != nil {
		t.Fatalf("accessToken() second error = %v", err)
	}
	if got != "cached-token" {
		t.Fatalf("accessToken() second = %q, want the in-memory token", got)
	}
}

// 刷新失败时降级：Redis 里临期但未过期的 token 仍要顶上，尤其是刚启动、
// 内存缓存为空的实例——否则钉钉一抖动，发卡就全线报错。
func TestAccessTokenFallsBackToStaleRedisToken(t *testing.T) {
	s := newTestCardSender(t)
	s.tokenKey = "adk:access_token:test"
	ctx := context.Background()

	// 临期（进入刷新余量）但还有 60 秒寿命
	at := AccessToken{Token: "stale-token", ExpiredAt: time.Now().Unix() + 60}
	b, err := json.Marshal(at)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := s.reduc.Set(ctx, s.tokenKey, string(b), time.Hour).Err(); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	refreshErr := errors.New("dingtalk unavailable")
	s.fetchToken = func(context.Context) (AccessToken, error) {
		return AccessToken{}, refreshErr
	}

	got, err := s.accessToken(ctx)
	if err != nil {
		t.Fatalf("accessToken() error = %v, want the stale token as fallback", err)
	}
	if got != "stale-token" {
		t.Fatalf("accessToken() = %q, want the stale redis token", got)
	}

	// 刷新恢复后要换上新 token，而不是抱着降级的旧 token 用到过期。
	// 手动让静默期过期，模拟 tokenRetryBackoff 之后的第一次调用。
	s.lastFetchErrAt = time.Now().Add(-tokenRetryBackoff)
	s.fetchToken = func(context.Context) (AccessToken, error) {
		return AccessToken{Token: "fresh-token", ExpiredAt: time.Now().Unix() + 7200}, nil
	}
	got, err = s.accessToken(ctx)
	if err != nil {
		t.Fatalf("accessToken() after recovery error = %v", err)
	}
	if got != "fresh-token" {
		t.Fatalf("accessToken() = %q, want the refreshed token", got)
	}
}

// 刷新失败后的静默期内不再试探端点：并发和后续调用直接降级或复报上次错误，
// 不会每次都把锁内超时走满、排成 10 秒一个的队列。
func TestAccessTokenBacksOffAfterRefreshFailure(t *testing.T) {
	s := newTestCardSender(t)
	s.tokenKey = "adk:access_token:test"

	fetches := 0
	s.fetchToken = func(context.Context) (AccessToken, error) {
		fetches++
		return AccessToken{}, errors.New("dingtalk unavailable")
	}

	if _, err := s.accessToken(context.Background()); err == nil {
		t.Fatal("accessToken() error = nil, want the refresh failure surfaced")
	}
	if _, err := s.accessToken(context.Background()); err == nil {
		t.Fatal("accessToken() second error = nil, want the failure replayed from backoff")
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1 (calls inside the backoff window must not probe the endpoint)", fetches)
	}

	// 静默期内手里有未过期 token 时直接降级，同样不试探
	s.token = AccessToken{Token: "degraded", ExpiredAt: time.Now().Unix() + 60}
	got, err := s.accessToken(context.Background())
	if err != nil || got != "degraded" {
		t.Fatalf("accessToken() = (%q, %v), want the degraded token without probing", got, err)
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d, want still 1 after the degraded hit", fetches)
	}
}

// 缓存全空且刷新失败时必须报错，不能拿空 token 去发卡。
func TestAccessTokenFailsWithoutAnyUsableToken(t *testing.T) {
	s := newTestCardSender(t)
	s.tokenKey = "adk:access_token:test"
	s.fetchToken = func(context.Context) (AccessToken, error) {
		return AccessToken{}, errors.New("dingtalk unavailable")
	}

	if _, err := s.accessToken(context.Background()); err == nil {
		t.Fatal("accessToken() error = nil, want the refresh failure surfaced")
	}
}

// 进入刷新余量的 token 不算可用，避免拿着临期 token 发卡失败；
// 但在降级场景（余量 0）下，未过期就仍然可用。
func TestAccessTokenFreshness(t *testing.T) {
	nearExpiry := AccessToken{Token: "t", ExpiredAt: time.Now().Unix() + 60}
	if nearExpiry.fresh(tokenRefreshMargin) {
		t.Error("a token inside the refresh margin must not count as fresh")
	}
	if !nearExpiry.fresh(0) {
		t.Error("a not-yet-expired token must count as fresh without margin")
	}

	expired := AccessToken{Token: "t", ExpiredAt: time.Now().Unix() - 1}
	if expired.fresh(0) {
		t.Error("an expired token must never count as fresh")
	}
	if (AccessToken{}).fresh(0) {
		t.Error("an empty token must never count as fresh")
	}
}
