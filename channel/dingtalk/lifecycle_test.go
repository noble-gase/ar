package dingtalk

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// launch 是唯一的异步入口。handler 自己的 recover 只能盖住拿到 outTrackId 之后的
// 部分，在那之前 panic 会逃逸出 goroutine 打挂整个进程。
func TestLaunchContainsPanic(t *testing.T) {
	b := newTestBot(newFakeCard(), &fakeChat{})

	if !b.launch(context.Background(), "boom", func(context.Context) {
		panic("boom")
	}) {
		t.Fatal("launch() = false, want the work to start")
	}

	b.Stop()
}

// 停机不能把在途消息拦腰截断：中途取消会在会话里留下半截事件。
func TestStopDrainsInFlightWork(t *testing.T) {
	b := newTestBot(newFakeCard(), &fakeChat{})

	var finished atomic.Bool
	started := make(chan struct{})
	if !b.launch(context.Background(), "slow", func(runCtx context.Context) {
		close(started)
		select {
		case <-time.After(50 * time.Millisecond):
			finished.Store(true)
		case <-runCtx.Done():
		}
	}) {
		t.Fatal("launch() = false, want the work to start")
	}

	<-started
	b.Stop()

	if !finished.Load() {
		t.Error("Stop() returned before the in-flight work finished")
	}
}

// 自然排空有上限：超过宽限期就发取消，不能让一个慢任务无限拖住发布。
// 注意 Stop 整体并不保证有界——取消之后仍会等 handler 真正退出。
func TestStopCancelsWorkBeyondTheGrace(t *testing.T) {
	b := newTestBot(newFakeCard(), &fakeChat{})
	b.shutdownGrace = 20 * time.Millisecond

	cancelled := make(chan struct{})
	if !b.launch(context.Background(), "stuck", func(runCtx context.Context) {
		<-runCtx.Done()
		close(cancelled)
	}) {
		t.Fatal("launch() = false, want the work to start")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Stop()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() blocked past the grace period without cancelling")
	}

	select {
	case <-cancelled:
	default:
		t.Error("Stop() returned without cancelling the stuck work")
	}
}

// 忽略取消的任务照样要等到它退出：Go 杀不死 goroutine，Stop 提前返回会让残留
// 任务在进程退出的边缘继续写卡片或会话。真正的强制退出交给进程管理器。
func TestStopWaitsForWorkThatIgnoresCancellation(t *testing.T) {
	b := newTestBot(newFakeCard(), &fakeChat{})
	b.shutdownGrace = 10 * time.Millisecond

	var finished atomic.Bool
	started := make(chan struct{})
	if !b.launch(context.Background(), "stubborn", func(context.Context) {
		close(started)
		time.Sleep(80 * time.Millisecond)
		finished.Store(true)
	}) {
		t.Fatal("launch() = false, want the work to start")
	}

	<-started
	b.Stop()

	if !finished.Load() {
		t.Error("Stop() returned while uncancellable work was still running")
	}
}

func TestLaunchRejectedAfterStop(t *testing.T) {
	b := newTestBot(newFakeCard(), &fakeChat{})
	b.Stop()

	if b.launch(context.Background(), "late", func(context.Context) {}) {
		t.Error("launch() = true after Stop, want new work refused")
	}
}

// Bot 是一次性的：Stop 之后重新 Start 会拿到一个已经不再接收任务的实例，
// 与其静默假装启动成功，不如明确报错。
func TestStartAfterStopFails(t *testing.T) {
	b := newTestBot(newFakeCard(), &fakeChat{})
	b.Stop()

	if err := b.Start(); !errors.Is(err, ErrStopped) {
		t.Errorf("Start() error = %v, want ErrStopped", err)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	b := newTestBot(newFakeCard(), &fakeChat{})

	b.Stop()
	b.Stop()
}

// launch 的 ctx 只借用 trace 等值，不能继承回调的取消：钉钉的回调 ctx 在回调返回
// 后就结束了，后台任务还得继续跑。
func TestLaunchOutlivesCallbackContext(t *testing.T) {
	b := newTestBot(newFakeCard(), &fakeChat{})

	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	if !b.launch(ctx, "detached", func(runCtx context.Context) {
		ran <- runCtx.Err()
	}) {
		t.Fatal("launch() = false, want the work to start")
	}
	cancel()

	select {
	case err := <-ran:
		if err != nil {
			t.Errorf("runCtx.Err() = %v, want the work detached from the callback context", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("launched work never ran")
	}
	b.Stop()
}
