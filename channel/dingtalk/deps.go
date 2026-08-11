package dingtalk

import (
	"context"
	"iter"

	"github.com/noble-gase/argon/llmchat"
	"google.golang.org/adk/v2/session"
)

// cardStore 是 Bot 依赖的卡片接口。抽出它是为了在没有钉钉客户端、也没有真实
// Redis 的情况下也能覆盖各条编排分支。*CardSender 实现了它。
//
// 待回答问题与待确认的工具调用不在这里：它们由 ADK 会话重建（chatClient.Pending），渠道
// 侧不再维护副本，也就没有缓存一致性问题。这里只保留会话里没有的东西——确认卡片的
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
	clearConfirms(ctx context.Context, userId string) ([]string, error)

	// lockUser 让同一个用户的消息在所有 bot 实例间串行化。锁一旦丢失，返回的
	// context 会被取消。
	lockUser(ctx context.Context, userId string) (context.Context, func(), error)
}

// chatClient 是 Bot 依赖的会话接口。*llmchat.Chat 实现了它。
type chatClient interface {
	Ask(ctx context.Context, userId, text string) (string, iter.Seq2[*session.Event, error], error)
	Reply(ctx context.Context, userId, interruptId string, payload any) (string, iter.Seq2[*session.Event, error], error)
	Confirm(ctx context.Context, userId, conversationId, callId string, approved bool, payload any) (iter.Seq2[*session.Event, error], error)
	Pending(ctx context.Context, userId string) (*llmchat.Pending, error)
	ResetAutomatic(ctx context.Context, userId string) error
}

var (
	_ cardStore  = (*CardSender)(nil)
	_ chatClient = (*llmchat.Chat)(nil)
)
