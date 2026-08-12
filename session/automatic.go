package session

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// autoConversation 是一个用户当前自动会话的指针记录。
//
// 自动会话不再由 (应用, 用户, 日期) 确定性派生：派生把轮换策略藏进了 UUID 里，
// 让会话边界依赖服务器时区，还让同日重置后 ID 不变，催生出「旧 callId 已消失」
// 这类专门的失效分支。指针把这些都换成一行记录：轮换是显式的条件更新，
// 重置是换指针，重置后 ID 必然全新。
type autoConversation struct {
	AppName        string `gorm:"primaryKey;size:32"`
	UserID         string `gorm:"primaryKey;size:64"`
	ConversationID string `gorm:"size:64;not null"`
	// RotatedOn 是指针最近一次轮换所在的自然日（YYYY-MM-DD，时区由 Session
	// 配置），决定跨日后是否轮换到新会话。
	RotatedOn string    `gorm:"size:16;not null"`
	UpdatedAt time.Time `gorm:"not null"`
}

func (autoConversation) TableName() string {
	return "auto_conversations"
}

type autoConversationRepository struct {
	db *gorm.DB
}

// Current 返回 userId 在 day 的自动会话 ID；指针缺失或日期落后于 day 时轮换到
// 新的 UUID。
//
// 轮换是单调的：RotatedOn >= day 一律复用（YYYY-MM-DD 字典序即时间序）。多实例
// 时区配置不一致或时钟回拨时，非单调的轮换会让实例间各自用新会话来回踩踏、把
// 用户的对话撕成碎片；单调比较把这种配置错误的后果收敛为「稳定在较大的日期」。
//
// 并发轮换靠条件写收敛：插入撞主键、条件更新没改到行，都说明输给了并发者，
// 重读一次即可拿到胜者写入的 ID。正常情况下第二轮必然命中，限三轮是防御。
func (r *autoConversationRepository) Current(ctx context.Context, appName, userId, day string) (string, error) {
	for range 3 {
		var ac autoConversation
		err := r.db.WithContext(ctx).
			Where("app_name = ? AND user_id = ?", appName, userId).
			First(&ac).Error

		switch {
		case err == nil && ac.RotatedOn >= day:
			return ac.ConversationID, nil
		case err == nil:
			// 向前轮换：条件更新保证只有仍停留在旧日期的那一行会被改写
			newId := uuid.NewString()
			result := r.db.WithContext(ctx).Model(&autoConversation{}).
				Where("app_name = ? AND user_id = ? AND rotated_on = ?", appName, userId, ac.RotatedOn).
				Updates(map[string]any{"conversation_id": newId, "rotated_on": day})
			if result.Error != nil {
				return "", result.Error
			}
			if result.RowsAffected == 1 {
				return newId, nil
			}
		case errors.Is(err, gorm.ErrRecordNotFound):
			newId := uuid.NewString()
			createErr := r.db.WithContext(ctx).Create(&autoConversation{
				AppName:        appName,
				UserID:         userId,
				ConversationID: newId,
				RotatedOn:      day,
			}).Error
			if createErr == nil {
				return newId, nil
			}
			if !errors.Is(createErr, gorm.ErrDuplicatedKey) {
				return "", createErr
			}
		default:
			return "", err
		}
	}
	return "", errors.New("session: automatic conversation pointer did not converge")
}

// CurrentID 返回指针当前指向的会话 ID，从不轮换；指针不存在时返回空串。
func (r *autoConversationRepository) CurrentID(ctx context.Context, appName, userId string) (string, error) {
	var ac autoConversation
	err := r.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ?", appName, userId).
		First(&ac).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return ac.ConversationID, nil
}

// Rotate 无条件把指针换成全新的 UUID，用于放弃当前自动会话。
// 旧会话的清理由调用方（ResetAutomatic）负责，这里只负责摘指针。
func (r *autoConversationRepository) Rotate(ctx context.Context, appName, userId, day string) error {
	newId := uuid.NewString()
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "app_name"}, {Name: "user_id"}},
		// OnConflict 的自定义赋值不会被 GORM 自动补 updated_at，需显式带上
		DoUpdates: clause.Assignments(map[string]any{
			"conversation_id": newId,
			"rotated_on":      day,
			"updated_at":      time.Now(),
		}),
	}).Create(&autoConversation{
		AppName:        appName,
		UserID:         userId,
		ConversationID: newId,
		RotatedOn:      day,
	}).Error
}
