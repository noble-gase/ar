package dingtalk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	// defaultLockTTL 决定实例崩溃后同一用户被阻塞多久，因此取值要短，长任务靠续期覆盖。
	defaultLockTTL = 30 * time.Second

	// defaultLockRenew 必须明显小于 TTL，留出重试余量。
	defaultLockRenew = 10 * time.Second

	// defaultLockRetry 是抢锁失败后的重试间隔。
	defaultLockRetry = 100 * time.Millisecond

	// defaultLockWait 是等待上一条消息释放用户锁的时长上限。它必须远小于运行
	// 超时（默认 1 小时）：上一条消息可能要跑几十分钟，让排队消息的卡片干等到
	// 运行超时才提示「正忙」毫无意义，超过这个时长就该尽早告知用户稍后再试。
	defaultLockWait = 30 * time.Second
)

// errUserBusy 表示等待用户锁超时：上一条消息仍在处理中。它是正常排队而不是
// 故障，调用方应提示用户稍后重试，而不是按基础设施错误处理。
var errUserBusy = errors.New("dingtalk: previous message still processing")

// releaseUserLock 只删除自己持有的锁：持锁期间若因故障超时失效、锁已被他人接手，
// 无条件 DEL 会把别人的锁删掉。
var releaseUserLock = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('DEL', KEYS[1])
end
return 0
`)

// renewUserLock 同样先校验持有者再续期。
var renewUserLock = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0
`)

func (s *CardSender) userLockKey(userId string) string {
	return fmt.Sprintf("adk:userlock:dingtalk:%s:%s", s.clientId, userId)
}

func (s *CardSender) lockTTL() time.Duration {
	if s.lockTTLOverride > 0 {
		return s.lockTTLOverride
	}
	return defaultLockTTL
}

func (s *CardSender) lockRenew() time.Duration {
	if s.lockRenewOverride > 0 {
		return s.lockRenewOverride
	}
	return defaultLockRenew
}

func (s *CardSender) lockRetry() time.Duration {
	if s.lockRetryOverride > 0 {
		return s.lockRetryOverride
	}
	return defaultLockRetry
}

func (s *CardSender) lockWait() time.Duration {
	if s.lockWaitOverride > 0 {
		return s.lockWaitOverride
	}
	return defaultLockWait
}

// lockUser 跨实例串行化同一用户的消息处理：两条消息并发驱动同一个 ADK session
// 会让事件交错、待回答状态互相覆盖。
//
// 阻塞直到抢到锁、等待超出 lockWait（返回 errUserBusy）或 ctx 结束。等待上限
// 独立于 ctx 的运行超时：排队的消息应尽早得到「正忙」的答复，而不是陪着上一条
// 消息跑完全程。返回的 context 在锁失去所有权时被取消——续期发现锁已被别人接手，
// 或长时间续不上导致锁必然已过期——调用方据此中止，避免和新的持锁者并发写同一个
// 会话。释放函数会停止续期并只删除自己持有的锁。
func (s *CardSender) lockUser(ctx context.Context, userId string) (context.Context, func(), error) {
	key := s.userLockKey(userId)
	token := uuid.NewString()
	ttl, renew, retry := s.lockTTL(), s.lockRenew(), s.lockRetry()

	waitTimer := time.NewTimer(s.lockWait())
	defer waitTimer.Stop()

	for {
		ok, err := s.reduc.SetNX(ctx, key, token, ttl).Result()
		if err != nil {
			return nil, nil, err
		}
		if ok {
			break
		}

		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-waitTimer.C:
			return nil, nil, errUserBusy
		case <-time.After(retry):
		}
	}

	// 单条消息可能跑很久（LLM + 工具调用），持锁期间要不断续期，
	// 否则锁提前过期会让另一条消息插进来
	heldCtx, lost := context.WithCancel(ctx)
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)

		ticker := time.NewTicker(renew)
		defer ticker.Stop()

		lastRenew := time.Now()
		for {
			select {
			case <-heldCtx.Done():
				return
			case <-ticker.C:
			}

			ok, err := renewUserLock.Run(heldCtx, s.reduc, []string{key}, token, ttl.Milliseconds()).Int64()
			switch {
			case err == nil && ok == 1:
				lastRenew = time.Now()
			case err == nil:
				// 锁已不在自己手里，继续跑就会和新持锁者并发写同一个会话
				slog.ErrorContext(heldCtx, "[dingtalk lock] user lock taken over, aborting", slog.String("userId", userId))
				lost()
				return
			case errors.Is(err, context.Canceled):
				return
			default:
				// 续期出错可能是瞬时的，但只要超过一个 TTL 没续上，锁必然已经过期
				if time.Since(lastRenew) >= ttl {
					slog.ErrorContext(heldCtx, "[dingtalk lock] user lock expired, aborting", slog.String("error", err.Error()), slog.String("userId", userId))
					lost()
					return
				}
				slog.WarnContext(heldCtx, "[dingtalk lock] renew user lock failed, will retry", slog.String("error", err.Error()), slog.String("userId", userId))
			}
		}
	}()

	release := func() {
		lost()
		<-renewDone

		// 释放不能受调用方 ctx 影响：ctx 已超时的话锁会一直滞留到 TTL 到期
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		if err := releaseUserLock.Run(releaseCtx, s.reduc, []string{key}, token).Err(); err != nil && !errors.Is(err, redis.Nil) {
			slog.ErrorContext(releaseCtx, "[dingtalk lock] release user lock failed", slog.String("error", err.Error()), slog.String("userId", userId))
		}
	}
	return heldCtx, release, nil
}
