package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ConversationStatus string

const (
	ConversationCreating ConversationStatus = "creating"
	ConversationActive   ConversationStatus = "active"
	ConversationDeleting ConversationStatus = "deleting"
	ConversationDeleted  ConversationStatus = "deleted"
	ConversationFailed   ConversationStatus = "failed"
)

// Conversation is the product-facing metadata for an explicit chat thread.
type Conversation struct {
	AppName            string             `gorm:"primaryKey;size:128;index:idx_conversation_list,priority:1" json:"appName"`
	UserID             string             `gorm:"primaryKey;size:128;index:idx_conversation_list,priority:2" json:"userId"`
	ConversationID     string             `gorm:"primaryKey;size:128;index:idx_conversation_list,priority:5,sort:desc" json:"conversationId"`
	Title              string             `gorm:"size:255" json:"title,omitempty"`
	Status             ConversationStatus `gorm:"size:16;not null;index:idx_conversation_list,priority:3" json:"status"`
	Version            uint64             `gorm:"not null;default:1" json:"version"`
	ReconcileAttempts  uint32             `gorm:"not null;default:0" json:"reconcileAttempts,omitempty"`
	LastReconcileError string             `gorm:"size:2048" json:"lastReconcileError,omitempty"`
	CreatedAt          time.Time          `gorm:"not null" json:"createdAt"`
	UpdatedAt          time.Time          `gorm:"not null;index:idx_conversation_list,priority:4,sort:desc" json:"updatedAt"`
	DeletedAt          *time.Time         `json:"deletedAt,omitempty"`
}

func (Conversation) TableName() string {
	return "conversations"
}

type ConversationPage struct {
	Items      []Conversation `json:"items"`
	NextCursor string         `json:"nextCursor,omitempty"`
}

const (
	autoConversationCreating = "creating"
	autoConversationActive   = "active"
)

var errAutoConversationClaimLost = errors.New("auto conversation claim lost")

// autoConversation points at the current automatic conversation for a user.
type autoConversation struct {
	AppName        string `gorm:"primaryKey;size:128"`
	UserID         string `gorm:"primaryKey;size:128"`
	ConversationID string `gorm:"size:128;not null"`
	Status         string `gorm:"size:16;not null;default:active"`
	// LeaseToken/LeaseUntil fence the creation critical section only: they are
	// set while Status is creating and cleared once active, since a live pointer
	// is stable and needs no lease. A crashed creator's lease expires so another
	// caller can take over; the stale creator's token then no longer matches.
	LeaseToken string `gorm:"size:64"`
	LeaseUntil time.Time
	UpdatedAt  time.Time `gorm:"not null"`
}

func (autoConversation) TableName() string {
	return "auto_conversations"
}

type ConversationRepository interface {
	Create(context.Context, *Conversation) error
	Get(context.Context, string, string, string) (*Conversation, error)
	Owns(context.Context, string, string, string) error
	Touch(context.Context, string, string, string, time.Time) error
	List(context.Context, string, string, string, int) (*ConversationPage, error)
	Pending(context.Context, string, int) ([]Conversation, error)
	RecordReconcileFailure(context.Context, string, string, string, ConversationStatus, string, uint32, time.Time) error
	BeginDelete(context.Context, string, string, string) error
	Transition(context.Context, string, string, string, ConversationStatus, ConversationStatus) error
	DeleteHard(context.Context, string, string, string) error
	ClaimAuto(context.Context, string, string, string, string, time.Time, time.Time, time.Time) (string, bool, error)
	CompleteAuto(context.Context, string, string, string, string, time.Time) error
	ReleaseAuto(context.Context, string, string, string) error
}

type gormConversationRepository struct {
	db *gorm.DB
}

func NewConversationRepository(dialector gorm.Dialector, opts ...gorm.Option) (ConversationRepository, error) {
	db, err := gorm.Open(dialector, opts...)
	if err != nil {
		return nil, err
	}

	if err := db.AutoMigrate(&Conversation{}, &autoConversation{}); err != nil {
		return nil, fmt.Errorf("migrate conversation metadata: %w", err)
	}
	return &gormConversationRepository{db: db}, nil
}

func (r *gormConversationRepository) Get(ctx context.Context, appName, userID, conversationID string) (*Conversation, error) {
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

func (r *gormConversationRepository) Create(ctx context.Context, conversation *Conversation) error {
	if err := r.db.WithContext(ctx).Create(conversation).Error; err != nil {
		return fmt.Errorf("create conversation metadata: %w", err)
	}
	return nil
}

func (r *gormConversationRepository) Owns(ctx context.Context, appName, userID, conversationID string) error {
	var count int64
	err := r.db.WithContext(ctx).Model(&Conversation{}).
		Where("app_name = ? AND user_id = ? AND conversation_id = ? AND status = ?", appName, userID, conversationID, ConversationActive).
		Count(&count).Error
	if err != nil {
		return fmt.Errorf("check conversation ownership: %w", err)
	}
	if count == 0 {
		return ErrConversationNotFound
	}
	return nil
}

func (r *gormConversationRepository) Touch(ctx context.Context, appName, userID, conversationID string, now time.Time) error {
	result := r.db.WithContext(ctx).Model(&Conversation{}).
		Where("app_name = ? AND user_id = ? AND conversation_id = ? AND status = ?", appName, userID, conversationID, ConversationActive).
		Updates(map[string]any{"updated_at": now, "version": gorm.Expr("version + 1")})
	if result.Error != nil {
		return fmt.Errorf("touch conversation: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrConversationNotFound
	}
	return nil
}

type conversationCursor struct {
	UpdatedAt time.Time `json:"updatedAt"`
	ID        string    `json:"id"`
}

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

func encodeConversationCursor(conversation Conversation) (string, error) {
	data, err := json.Marshal(conversationCursor{UpdatedAt: conversation.UpdatedAt, ID: conversation.ConversationID})
	if err != nil {
		return "", fmt.Errorf("encode conversation cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func (r *gormConversationRepository) List(ctx context.Context, appName, userID, cursorValue string, limit int) (*ConversationPage, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	cursor, err := decodeConversationCursor(cursorValue)
	if err != nil {
		return nil, err
	}

	query := r.db.WithContext(ctx).Where(
		"app_name = ? AND user_id = ? AND status = ?", appName, userID, ConversationActive,
	)
	if cursor != nil {
		query = query.Where(
			"updated_at < ? OR (updated_at = ? AND conversation_id < ?)",
			cursor.UpdatedAt, cursor.UpdatedAt, cursor.ID,
		)
	}

	var conversations []Conversation
	if _err := query.Order("updated_at DESC").Order("conversation_id DESC").Limit(limit + 1).Find(&conversations).Error; _err != nil {
		return nil, fmt.Errorf("list conversations: %w", _err)
	}

	page := &ConversationPage{Items: conversations}
	if len(conversations) > limit {
		page.Items = conversations[:limit]
		page.NextCursor, err = encodeConversationCursor(page.Items[len(page.Items)-1])
		if err != nil {
			return nil, err
		}
	}
	return page, nil
}

func (r *gormConversationRepository) Pending(ctx context.Context, appName string, limit int) ([]Conversation, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var conversations []Conversation
	err := r.db.WithContext(ctx).
		Where("app_name = ? AND status IN ?", appName, []ConversationStatus{ConversationCreating, ConversationDeleting}).
		Order("updated_at ASC").Limit(limit).Find(&conversations).Error
	if err != nil {
		return nil, fmt.Errorf("list pending conversations: %w", err)
	}
	return conversations, nil
}

func (r *gormConversationRepository) RecordReconcileFailure(ctx context.Context, appName, userID, conversationID string, status ConversationStatus, message string, maxAttempts uint32, now time.Time) error {
	const maxErrorRunes = 2048
	if value := []rune(message); len(value) > maxErrorRunes {
		message = string(value[:maxErrorRunes])
	}

	result := r.db.WithContext(ctx).Model(&Conversation{}).
		Where("app_name = ? AND user_id = ? AND conversation_id = ? AND status = ?", appName, userID, conversationID, status).
		Updates(map[string]any{
			"reconcile_attempts":   gorm.Expr("reconcile_attempts + 1"),
			"last_reconcile_error": message,
			"updated_at":           now,
		})
	if result.Error != nil {
		return fmt.Errorf("record reconcile failure: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil
	}

	// Only a creating conversation may become terminal. A deleting one must keep
	// retrying: giving up would orphan backend data with no way to re-delete.
	if status != ConversationCreating {
		return nil
	}
	result = r.db.WithContext(ctx).Model(&Conversation{}).
		Where("app_name = ? AND user_id = ? AND conversation_id = ? AND status = ? AND reconcile_attempts >= ?", appName, userID, conversationID, status, maxAttempts).
		Updates(map[string]any{"status": ConversationFailed, "version": gorm.Expr("version + 1")})
	if result.Error != nil {
		return fmt.Errorf("terminate reconcile retries: %w", result.Error)
	}
	return nil
}

func (r *gormConversationRepository) BeginDelete(ctx context.Context, appName, userID, conversationID string) error {
	err := r.Transition(ctx, appName, userID, conversationID, ConversationActive, ConversationDeleting)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrConversationNotFound) {
		return err
	}
	conversation, getErr := r.Get(ctx, appName, userID, conversationID)
	if getErr != nil {
		return getErr
	}
	if conversation.Status == ConversationDeleting {
		return nil
	}
	return ErrConversationNotFound
}

func (r *gormConversationRepository) Transition(ctx context.Context, appName, userID, conversationID string, from, to ConversationStatus) error {
	updates := map[string]any{
		"status":               to,
		"reconcile_attempts":   0,
		"last_reconcile_error": "",
		"updated_at":           time.Now(),
		"version":              gorm.Expr("version + 1"),
	}
	if to == ConversationDeleted {
		now := time.Now()
		updates["deleted_at"] = &now
	}
	result := r.db.WithContext(ctx).Model(&Conversation{}).
		Where("app_name = ? AND user_id = ? AND conversation_id = ? AND status = ?", appName, userID, conversationID, from).
		Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("transition conversation from %s to %s: %w", from, to, result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrConversationNotFound
	}
	return nil
}

func (r *gormConversationRepository) DeleteHard(ctx context.Context, appName, userID, conversationID string) error {
	return r.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND conversation_id = ?", appName, userID, conversationID).
		Delete(&Conversation{}).Error
}

func (r *gormConversationRepository) ClaimAuto(ctx context.Context, appName, userID, token, candidateID string, now, staleBefore, leaseUntil time.Time) (string, bool, error) {
	var record autoConversation
	err := r.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ?", appName, userID).
		First(&record).Error
	if err == nil && record.Status == autoConversationActive && !record.UpdatedAt.Before(staleBefore) {
		result := r.db.WithContext(ctx).Model(&autoConversation{}).
			Where("app_name = ? AND user_id = ? AND status = ? AND conversation_id = ? AND updated_at >= ?",
				appName, userID, autoConversationActive, record.ConversationID, staleBefore).
			Update("updated_at", now)
		if result.Error != nil {
			return "", false, fmt.Errorf("touch auto conversation: %w", result.Error)
		}
		if result.RowsAffected == 1 {
			return record.ConversationID, false, nil
		}
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, fmt.Errorf("get auto conversation: %w", err)
	}

	// Persist the candidate id with the claim so a crash mid-create leaves a
	// recoverable pointer instead of an untraceable orphan ADK session.
	claim := &autoConversation{
		AppName: appName, UserID: userID, ConversationID: candidateID,
		Status: autoConversationCreating, LeaseToken: token, LeaseUntil: leaseUntil, UpdatedAt: now,
	}
	result := r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(claim)
	if result.Error != nil {
		return "", false, fmt.Errorf("insert auto conversation claim: %w", result.Error)
	}
	if result.RowsAffected == 1 {
		return candidateID, true, nil
	}

	if err = r.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ?", appName, userID).
		First(&record).Error; err != nil {
		return "", false, fmt.Errorf("get auto conversation claim: %w", err)
	}

	// Keep the persisted candidate id across takeovers so every possible
	// committed create remains recoverable.
	if record.Status == autoConversationCreating && record.LeaseUntil.Before(now) {
		result = r.db.WithContext(ctx).Model(&autoConversation{}).
			Where("app_name = ? AND user_id = ? AND status = ? AND lease_token = ? AND lease_until < ?",
				appName, userID, autoConversationCreating, record.LeaseToken, now).
			Updates(map[string]any{
				"lease_token": token, "lease_until": leaseUntil, "updated_at": now,
			})
		if result.Error != nil {
			return "", false, fmt.Errorf("take over auto conversation claim: %w", result.Error)
		}
		if result.RowsAffected == 1 {
			return record.ConversationID, true, nil
		}
	}

	// Rotate away a stale active pointer. The old session is retained history,
	// not an orphan, so it is left in place.
	if record.Status == autoConversationActive && record.UpdatedAt.Before(staleBefore) {
		result = r.db.WithContext(ctx).Model(&autoConversation{}).
			Where("app_name = ? AND user_id = ? AND status = ? AND conversation_id = ? AND updated_at < ?",
				appName, userID, autoConversationActive, record.ConversationID, staleBefore).
			Updates(map[string]any{
				"conversation_id": candidateID, "status": autoConversationCreating, "lease_token": token,
				"lease_until": leaseUntil, "updated_at": now,
			})
		if result.Error != nil {
			return "", false, fmt.Errorf("rotate auto conversation: %w", result.Error)
		}
		if result.RowsAffected == 1 {
			return candidateID, true, nil
		}
	}

	// Lost the race: reuse the winner's pointer if it is now active and fresh.
	if err = r.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ?", appName, userID).
		First(&record).Error; err != nil {
		return "", false, fmt.Errorf("get auto conversation claim: %w", err)
	}
	if record.Status == autoConversationActive {
		result = r.db.WithContext(ctx).Model(&autoConversation{}).
			Where("app_name = ? AND user_id = ? AND status = ? AND conversation_id = ? AND updated_at >= ?",
				appName, userID, autoConversationActive, record.ConversationID, staleBefore).
			Update("updated_at", now)
		if result.Error != nil {
			return "", false, fmt.Errorf("touch auto conversation: %w", result.Error)
		}
		if result.RowsAffected == 1 {
			return record.ConversationID, false, nil
		}
	}
	return "", false, nil
}

func (r *gormConversationRepository) CompleteAuto(ctx context.Context, appName, userID, token, conversationID string, now time.Time) error {
	result := r.db.WithContext(ctx).Model(&autoConversation{}).
		Where("app_name = ? AND user_id = ? AND conversation_id = ? AND status = ? AND lease_token = ?",
			appName, userID, conversationID, autoConversationCreating, token).
		Updates(map[string]any{
			"status": autoConversationActive, "lease_token": "",
			"lease_until": time.Time{}, "updated_at": now,
		})
	if result.Error != nil {
		return fmt.Errorf("complete auto conversation: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return errAutoConversationClaimLost
	}
	return nil
}

func (r *gormConversationRepository) ReleaseAuto(ctx context.Context, appName, userID, token string) error {
	result := r.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND status = ? AND lease_token = ?", appName, userID, autoConversationCreating, token).
		Delete(&autoConversation{})
	if result.Error != nil {
		return fmt.Errorf("release auto conversation claim: %w", result.Error)
	}
	return nil
}
