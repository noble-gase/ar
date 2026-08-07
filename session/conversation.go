package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

var (
	ErrConversationNotFound  = errors.New("conversation not found")
	ErrInvalidAppName        = errors.New("invalid app name")
	ErrInvalidUserID         = errors.New("invalid user id")
	ErrInvalidConversationID = errors.New("invalid conversation id")
	ErrTitleTooLong          = errors.New("conversation title too long")
)

const (
	maxAppNameRunes = 32
	maxUserIDRunes  = 64
	maxConvIDRunes  = 64
	maxTitleRunes   = 128
)

// Conversation 是面向产品层的显式会话元数据，也是会话对用户是否可见的唯一依据。
type Conversation struct {
	AppName        string    `gorm:"primaryKey;size:32;index:idx_conversation_list,priority:1" json:"appName"`
	UserID         string    `gorm:"primaryKey;size:64;index:idx_conversation_list,priority:2" json:"userId"`
	ConversationID string    `gorm:"primaryKey;size:64;index:idx_conversation_list,priority:4,sort:desc" json:"conversationId"`
	Title          string    `gorm:"size:128" json:"title,omitempty"`
	CreatedAt      time.Time `gorm:"not null" json:"createdAt"`
	UpdatedAt      time.Time `gorm:"not null;index:idx_conversation_list,priority:3,sort:desc" json:"updatedAt"`
}

func (Conversation) TableName() string {
	return "conversations"
}

type ConversationPage struct {
	Items      []Conversation `json:"items"`
	NextCursor string         `json:"nextCursor,omitempty"`
}

type ConversationRepository interface {
	Create(ctx context.Context, conversation *Conversation) error
	Get(ctx context.Context, appName, userID, conversationID string) (*Conversation, error)
	Touch(ctx context.Context, appName, userID, conversationID string, now time.Time) error
	Rename(ctx context.Context, appName, userID, conversationID, title string) error
	List(ctx context.Context, appName, userID, cursor string, limit int) (*ConversationPage, error)
	Delete(ctx context.Context, appName, userID, conversationID string) error
}

type conversationRepository struct {
	db *gorm.DB
}

func NewConversationRepository(dialector gorm.Dialector, opts ...gorm.Option) (ConversationRepository, error) {
	db, err := gorm.Open(dialector, opts...)
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&Conversation{}); err != nil {
		return nil, fmt.Errorf("migrate conversation metadata: %w", err)
	}
	return &conversationRepository{db: db}, nil
}

func (r *conversationRepository) Get(ctx context.Context, appName, userID, conversationID string) (*Conversation, error) {
	var conversation Conversation
	err := r.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND conversation_id = ?", appName, userID, conversationID).
		First(&conversation).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get conversation metadata: %w", err)
	}
	return &conversation, nil
}

func (r *conversationRepository) Create(ctx context.Context, conversation *Conversation) error {
	if err := r.db.WithContext(ctx).Create(conversation).Error; err != nil {
		return fmt.Errorf("create conversation metadata: %w", err)
	}
	return nil
}

func (r *conversationRepository) Touch(ctx context.Context, appName, userID, conversationID string, now time.Time) error {
	result := r.db.WithContext(ctx).Model(&Conversation{}).
		Where("app_name = ? AND user_id = ? AND conversation_id = ?", appName, userID, conversationID).
		Update("updated_at", now)
	if result.Error != nil {
		return fmt.Errorf("touch conversation: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrConversationNotFound
	}
	return nil
}

// Rename 在一次条件更新中校验归属关系并改写标题。
// UpdateColumn 跳过 GORM 自动时间戳，使重命名不会打乱按活跃度排序的列表。
func (r *conversationRepository) Rename(ctx context.Context, appName, userID, conversationID, title string) error {
	result := r.db.WithContext(ctx).Model(&Conversation{}).
		Where("app_name = ? AND user_id = ? AND conversation_id = ?", appName, userID, conversationID).
		UpdateColumn("title", title)
	if result.Error != nil {
		return fmt.Errorf("rename conversation: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrConversationNotFound
	}
	return nil
}

// Delete 移除元数据，会话由此对用户不可见。
func (r *conversationRepository) Delete(ctx context.Context, appName, userID, conversationID string) error {
	result := r.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND conversation_id = ?", appName, userID, conversationID).
		Delete(&Conversation{})
	if result.Error != nil {
		return fmt.Errorf("delete conversation metadata: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrConversationNotFound
	}
	return nil
}

// List 按 (updated_at, conversation_id) 降序 keyset 分页，多读一条用于判断是否需要续页游标。
func (r *conversationRepository) List(ctx context.Context, appName, userID, cursorValue string, limit int) (*ConversationPage, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	cursor, err := decodeConversationCursor(cursorValue)
	if err != nil {
		return nil, err
	}

	query := r.db.WithContext(ctx).Where("app_name = ? AND user_id = ?", appName, userID)
	if cursor != nil {
		// 排序为双字段降序，因此下一页必须同时处理时间更早和同时间但 ID 更小的记录。
		query = query.Where(
			"updated_at < ? OR (updated_at = ? AND conversation_id < ?)",
			cursor.UpdatedAt, cursor.UpdatedAt, cursor.ID,
		)
	}

	var conversations []Conversation
	err = query.Order("updated_at DESC").Order("conversation_id DESC").Limit(limit + 1).Find(&conversations).Error
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}

	page := &ConversationPage{Items: conversations}
	if len(conversations) > limit {
		// 多读取的一条仅用于证明还有下一页，不应返回给调用方。
		page.Items = conversations[:limit]
		page.NextCursor, err = encodeConversationCursor(page.Items[len(page.Items)-1])
		if err != nil {
			return nil, err
		}
	}
	return page, nil
}

// conversationCursor 保存一页末尾的复合排序键，UpdatedAt 相同时由 ID 决胜以保证遍历稳定。
type conversationCursor struct {
	UpdatedAt time.Time `json:"updatedAt"`
	ID        string    `json:"id"`
}

// decodeConversationCursor 解析不透明且 URL 安全的 keyset 游标。
// 空值表示第一页；格式错误的游标直接报错，不会静默重置分页。
func decodeConversationCursor(value string) (*conversationCursor, error) {
	if value == "" {
		return nil, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode conversation cursor: %w", err)
	}
	var cursor conversationCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return nil, fmt.Errorf("decode conversation cursor: %w", err)
	}
	if cursor.UpdatedAt.IsZero() || cursor.ID == "" {
		return nil, errors.New("invalid conversation cursor")
	}
	return &cursor, nil
}

// encodeConversationCursor 序列化排序键，避免向调用方暴露数据库特定格式。
func encodeConversationCursor(conversation Conversation) (string, error) {
	data, err := json.Marshal(conversationCursor{UpdatedAt: conversation.UpdatedAt, ID: conversation.ConversationID})
	if err != nil {
		return "", fmt.Errorf("encode conversation cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
