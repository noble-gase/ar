package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// confirmTTL 是确认记录的保留时长。
const confirmTTL = time.Hour

// pendingConfirm 是一次待人工确认的工具调用，按确认卡片的 outTrackId 存储。
//
// 它没有独立的租约或状态位：确认的整个处理过程都在用户锁内完成，记录存在即代表
// 「还可被认领」，删除即代表「已处理」。用户锁本身会续期、会在失去所有权时取消
// 执行，因此不需要第二套并发控制——两套锁生命周期不一致正是此前反复出问题的根源。
type pendingConfirm struct {
	CallId      string `json:"call_id"`
	UserId      string `json:"user_id"`
	ConvType    string `json:"conv_type"`
	GroupConvId string `json:"group_conv_id"`

	// SessionId 是发出这次确认的那次运行所在的会话，必须取自运行本身。自动会话跨
	// 自然日轮换，若在投卡时重新计算，横跨午夜的执行会把记录挂到第二天的会话上。
	SessionId string `json:"session_id"`

	// Prompt 是确认卡片的正文，恢复失败需要重新投卡时复用。
	Prompt string `json:"prompt,omitempty"`
}

func (s *CardSender) pendingKey(outTrackId string) string {
	return fmt.Sprintf("adk:confirm:dingtalk:%s:%s", s.clientId, outTrackId)
}

// userConfirmsKey 索引一个用户名下所有待确认记录，用于会话重置时统一失效。
func (s *CardSender) userConfirmsKey(userId string) string {
	return fmt.Sprintf("adk:confirm:user:dingtalk:%s:%s", s.clientId, userId)
}

// savePending 保存确认记录，并挂到用户索引下。
func (s *CardSender) savePending(ctx context.Context, outTrackId string, p *pendingConfirm) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}

	pipe := s.reduc.TxPipeline()
	pipe.Set(ctx, s.pendingKey(outTrackId), string(b), confirmTTL)
	pipe.SAdd(ctx, s.userConfirmsKey(p.UserId), outTrackId)
	pipe.Expire(ctx, s.userConfirmsKey(p.UserId), confirmTTL)
	_, err = pipe.Exec(ctx)
	return err
}

// loadPending 按确认卡片的 outTrackId 读取记录。
// 返回 redis.Nil 表示这次确认已被处理，或所属会话已被重置。
func (s *CardSender) loadPending(ctx context.Context, outTrackId string) (*pendingConfirm, error) {
	str, err := s.reduc.Get(ctx, s.pendingKey(outTrackId)).Result()
	if err != nil {
		return nil, err
	}

	var p pendingConfirm
	if err := json.Unmarshal([]byte(str), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// dropPending 删除确认记录，表示这次确认已处理完毕、不能再被认领。
func (s *CardSender) dropPending(ctx context.Context, outTrackId, userId string) error {
	pipe := s.reduc.TxPipeline()
	pipe.Del(ctx, s.pendingKey(outTrackId))
	pipe.SRem(ctx, s.userConfirmsKey(userId), outTrackId)
	_, err := pipe.Exec(ctx)
	return err
}

// clearConfirms 作废一个用户名下的全部确认记录。会话被重置后，旧卡片上的按钮不能
// 再去恢复一个已经不存在的工具调用。
func (s *CardSender) clearConfirms(ctx context.Context, userId string) error {
	indexKey := s.userConfirmsKey(userId)

	trackIds, err := s.reduc.SMembers(ctx, indexKey).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}

	pipe := s.reduc.TxPipeline()
	for _, id := range trackIds {
		pipe.Del(ctx, s.pendingKey(id))
	}
	pipe.Del(ctx, indexKey)
	_, err = pipe.Exec(ctx)
	return err
}
