// Package userlock 提供按 key（通常是 userId）互斥的分布式锁，把同一用户的
// 处理跨实例串行化。
//
// llmchat.Chat 的并发契约要求渠道层自备这把互斥：两条消息并发驱动同一个 ADK
// 会话会让事件交错、待回答状态互相覆盖。钉钉渠道即用本包实现，自建渠道可直接
// 复用，无需再写一遍续期与失联处理。
package userlock

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	// defaultTTL 决定持有实例崩溃后同一 key 被阻塞多久，因此取值要短，
	// 长任务靠续期覆盖。
	defaultTTL = 30 * time.Second

	// defaultRenew 必须明显小于 TTL，留出重试余量。
	defaultRenew = 10 * time.Second

	// defaultRetry 是抢锁失败后的重试间隔。
	defaultRetry = 100 * time.Millisecond

	// defaultWait 是等待锁释放的时长上限。它必须远小于业务的运行超时：持有方
	// 可能要跑很久，排队方应尽早得到「正忙」的答复，而不是陪着持有方跑完全程。
	defaultWait = 30 * time.Second

	// releaseTimeout 限制释放操作的时长。释放不能受调用方 ctx 影响：
	// ctx 已超时的话锁会一直滞留到 TTL 到期。
	releaseTimeout = 5 * time.Second
)

// ErrBusy 表示等待锁超时：上一次处理仍在进行中。它是正常排队而不是故障，
// 调用方应提示稍后重试，而不是按基础设施错误处理。
var ErrBusy = errors.New("userlock: lock is held by another owner")

// releaseScript 只删除自己持有的锁：持锁期间若因故障超时失效、锁已被他人接手，
// 无条件 DEL 会把别人的锁删掉。
var releaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('DEL', KEYS[1])
end
return 0
`)

// renewScript 同样先校验持有者再续期。
var renewScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0
`)

// Config 配置 Locker，零值字段使用默认值。
type Config struct {
	// Prefix 是锁键前缀，用于隔离不同应用/渠道
	// （如 "adk:userlock:dingtalk:<clientId>"）。为空时用 "argon:userlock"。
	Prefix string

	// TTL 决定持有实例崩溃后同一 key 被阻塞多久，取值要短，长任务靠续期覆盖。
	// 为零时 30 秒。
	TTL time.Duration

	// Renew 是续期间隔，必须明显小于 TTL，留出重试余量。为零时 10 秒；
	// 配置成 >= TTL 时会被钳到 TTL 的三分之一。
	Renew time.Duration

	// Retry 是抢锁失败后的重试间隔。为零时 100 毫秒。
	Retry time.Duration

	// Wait 是等待锁释放的时长上限，超过返回 ErrBusy。它应远小于业务的运行
	// 超时，让排队方尽早得到「正忙」的答复。为零时 30 秒。
	Wait time.Duration
}

// Locker 是按 key 互斥的分布式锁工厂，可被多个 goroutine 并发使用。
type Locker struct {
	client redis.UniversalClient
	prefix string

	ttl   time.Duration
	renew time.Duration
	retry time.Duration
	wait  time.Duration
}

// New 返回一个 Locker。client 由调用方持有并负责生命周期。
func New(client redis.UniversalClient, cfg Config) *Locker {
	l := &Locker{
		client: client,
		prefix: cfg.Prefix,
		ttl:    cfg.TTL,
		renew:  cfg.Renew,
		retry:  cfg.Retry,
		wait:   cfg.Wait,
	}
	if l.prefix == "" {
		l.prefix = "argon:userlock"
	}
	if l.ttl <= 0 {
		l.ttl = defaultTTL
	}
	if l.renew <= 0 {
		l.renew = defaultRenew
	}
	// 续期必须明显快于过期，否则锁会周期性失效、被别人插队——这正是本包要防
	// 的事，不能被一行配置绕过。钳到 TTL 的三分之一（与默认比例一致），留出
	// 续期失败后的重试余量。
	if l.renew >= l.ttl {
		l.renew = l.ttl / 3
	}
	if l.retry <= 0 {
		l.retry = defaultRetry
	}
	if l.wait <= 0 {
		l.wait = defaultWait
	}
	return l
}

func (l *Locker) key(key string) string {
	return l.prefix + ":" + key
}

// Lock 跨实例串行化对同一 key 的处理。
//
// 阻塞直到抢到锁、等待超出 Wait（返回 ErrBusy）或 ctx 结束。等待上限独立于
// ctx 的超时：排队方应尽早得到「正忙」的答复，而不是陪着持有方跑完全程。
//
// 返回的 context 派生自 ctx，并在锁失去所有权时被取消——续期发现锁已被别人
// 接手，或长时间续不上导致锁必然已过期——调用方据此中止，避免和新的持锁者
// 并发处理同一个 key。释放函数会停止续期并只删除自己持有的锁。
func (l *Locker) Lock(ctx context.Context, key string) (context.Context, func(), error) {
	lockKey := l.key(key)
	token := uuid.NewString()

	waitTimer := time.NewTimer(l.wait)
	defer waitTimer.Stop()

	for {
		ok, err := l.client.SetNX(ctx, lockKey, token, l.ttl).Result()
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
			return nil, nil, ErrBusy
		case <-time.After(l.retry):
		}
	}

	// 一次处理可能跑很久，持锁期间要不断续期，
	// 否则锁提前过期会让下一次处理插进来
	heldCtx, lost := context.WithCancel(ctx)
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)

		ticker := time.NewTicker(l.renew)
		defer ticker.Stop()

		lastRenew := time.Now()
		for {
			select {
			case <-heldCtx.Done():
				return
			case <-ticker.C:
			}

			ok, err := renewScript.Run(heldCtx, l.client, []string{lockKey}, token, l.ttl.Milliseconds()).Int64()
			switch {
			case err == nil && ok == 1:
				lastRenew = time.Now()
			case err == nil:
				// 锁已不在自己手里，继续跑就会和新持锁者并发处理同一个 key
				slog.ErrorContext(heldCtx, "[userlock] lock taken over, aborting", slog.String("key", key))
				lost()
				return
			case errors.Is(err, context.Canceled):
				return
			default:
				// 续期出错可能是瞬时的，但只要超过一个 TTL 没续上，锁必然已经过期
				if time.Since(lastRenew) >= l.ttl {
					slog.ErrorContext(heldCtx, "[userlock] lock expired, aborting", slog.String("error", err.Error()), slog.String("key", key))
					lost()
					return
				}
				slog.WarnContext(heldCtx, "[userlock] renew failed, will retry", slog.String("error", err.Error()), slog.String("key", key))
			}
		}
	}()

	release := func() {
		lost()
		<-renewDone

		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer cancel()

		if err := releaseScript.Run(releaseCtx, l.client, []string{lockKey}, token).Err(); err != nil && !errors.Is(err, redis.Nil) {
			slog.ErrorContext(releaseCtx, "[userlock] release failed", slog.String("error", err.Error()), slog.String("key", key))
		}
	}
	return heldCtx, release, nil
}
