package dingtalk

import (
	"context"
	"iter"

	"github.com/noble-gase/argon/llmchat"
	"google.golang.org/adk/v2/session"
)

// cardStore is the card surface the Bot depends on. It exists so the
// orchestration branches can be exercised without a DingTalk client or a live
// Redis. *CardSender implements it.
//
// 待回答问题不在这里：它们由 ADK 会话重建（chatClient.PendingInputs），渠道侧不再
// 维护副本，也就没有缓存一致性问题。这里只保留会话里没有的东西——确认卡片的
// outTrackId 到 callId 的映射，以及跨实例的用户锁。
type cardStore interface {
	Close()

	CreateAndDeliverRobot(ctx context.Context, userId string) (string, error)
	CreateAndDeliverGroup(ctx context.Context, userId, conversationId string) (string, error)
	NewOutTrackId() string
	DeliverConfirm(ctx context.Context, outTrackId string, meta msgMeta, content string) (string, error)
	StreamingUpdate(ctx context.Context, outTrackId, content string, finished bool)

	savePending(ctx context.Context, outTrackId string, p *pendingConfirm) error
	loadPending(ctx context.Context, outTrackId string) (*pendingConfirm, error)
	dropPending(ctx context.Context, outTrackId, userId string) error
	clearConfirms(ctx context.Context, userId string) error

	// lockUser serializes one user's messages across all bot instances. The
	// returned context is cancelled if the lock is lost.
	lockUser(ctx context.Context, userId string) (context.Context, func(), error)
}

// chatClient is the conversation surface the Bot depends on. *llmchat.Chat
// implements it.
type chatClient interface {
	Ask(ctx context.Context, userId, text string) (string, iter.Seq2[*session.Event, error], error)
	Reply(ctx context.Context, userId, interruptId string, payload any) (string, iter.Seq2[*session.Event, error], error)
	Confirm(ctx context.Context, userId, conversationId, callId string, approved bool, payload any) (iter.Seq2[*session.Event, error], error)
	PendingInputs(ctx context.Context, userId string) ([]*llmchat.RequestInput, error)
	ResetAutomatic(ctx context.Context, userId string) error
}

var (
	_ cardStore  = (*CardSender)(nil)
	_ chatClient = (*llmchat.Chat)(nil)
)
