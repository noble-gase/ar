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
	// ConversationCreating 表示元数据已存在，但尚不确定 ADK 创建是否提交。
	ConversationCreating ConversationStatus = "creating"
	// ConversationActive 是归属校验和列表接口唯一对外可见的状态。
	ConversationActive ConversationStatus = "active"
	// ConversationDeleting 在重试 ADK 删除期间对外隐藏会话。
	ConversationDeleting ConversationStatus = "deleting"
	// ConversationDeleted 是删除完成后的终态墓碑。
	ConversationDeleted ConversationStatus = "deleted"
	// ConversationFailed 是无法恢复的创建操作终态。
	ConversationFailed ConversationStatus = "failed"
)

// Conversation 是面向产品层的显式会话元数据。
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

// autoConversation 指向用户当前的自动会话。
type autoConversation struct {
	AppName        string `gorm:"primaryKey;size:128"`
	UserID         string `gorm:"primaryKey;size:128"`
	ConversationID string `gorm:"size:128;not null"`
	Status         string `gorm:"size:16;not null;default:active"`
	// LeaseToken 和 LeaseUntil 仅用于隔离创建临界区：Status 为 creating 时设置，
	// 进入 active 后清空，因为已激活的稳定指针不再需要租约。
	// 创建者崩溃后租约会过期，其他调用者可以接管，旧创建者的 token 将不再匹配。
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

// conversationCursor 保存一页数据末尾的复合排序键。
// 当多条记录的 UpdatedAt 相同时，ID 作为决胜字段保证遍历稳定。
type conversationCursor struct {
	UpdatedAt time.Time `json:"updatedAt"`
	ID        string    `json:"id"`
}

// decodeConversationCursor 校验不透明且 URL 安全的 keyset 游标。
// 空值表示第一页；格式错误或字段不完整的游标会直接报错，不会静默重置分页。
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

// encodeConversationCursor 序列化降序复合排序键，避免向调用方暴露数据库特定格式。
func encodeConversationCursor(conversation Conversation) (string, error) {
	data, err := json.Marshal(conversationCursor{UpdatedAt: conversation.UpdatedAt, ID: conversation.ConversationID})
	if err != nil {
		return "", fmt.Errorf("encode conversation cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// List 使用 (updated_at, conversation_id) 降序 keyset 分页返回 active 会话。
// 查询会额外读取一条记录以判断是否需要续页游标，无效的 limit 会回退到默认值。
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
		// 排序为双字段降序，因此下一页必须同时处理时间更早和同时间但 ID 更小的记录。
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
		// 多读取的一条仅用于证明还有下一页，不应返回给调用方。
		page.Items = conversations[:limit]
		page.NextCursor, err = encodeConversationCursor(page.Items[len(page.Items)-1])
		if err != nil {
			return nil, err
		}
	}
	return page, nil
}

// Pending 按最旧优先返回可恢复的中间状态。
// 记录协调失败时会推进 UpdatedAt，使前序操作持续失败时后续记录仍能得到处理。
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

// RecordReconcileFailure 仅在记录仍处于发生故障的状态时有条件地写入失败信息。
// creating 达到 maxAttempts 后进入终态，因为持续且结果不明的故障需要人工检查；
// deleting 则有意不设置终态，因为 ADK 删除具备幂等性，放弃重试会留下无法通过
// 公共接口再次删除的后端数据。
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
		// 状态已被并发推进时无需记录旧状态的失败，也不应覆盖新状态的诊断信息。
		return nil
	}

	// 只有 creating 会话可以因重试耗尽进入终态。deleting 必须持续重试，
	// 否则会留下无法再次删除的孤儿后端数据。
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

// BeginDelete 通过将 active 原子转换为 deleting 来隐藏会话。
// 对已处于 deleting 的记录重复调用也会成功，因此公共删除操作可在响应不确定后安全重试。
func (r *gormConversationRepository) BeginDelete(ctx context.Context, appName, userID, conversationID string) error {
	err := r.Transition(ctx, appName, userID, conversationID, ConversationActive, ConversationDeleting)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrConversationNotFound) {
		return err
	}
	// 条件转换未命中既可能是记录不存在，也可能是上一次删除已进入 deleting，
	// 因此重新读取以实现删除请求的幂等重试。
	conversation, getErr := r.Get(ctx, appName, userID, conversationID)
	if getErr != nil {
		return getErr
	}
	if conversation.Status == ConversationDeleting {
		return nil
	}
	return ErrConversationNotFound
}

// Transition 对 Status 执行比较并交换。RowsAffected 用于区分成功转换、记录不存在
// 和并发状态变化；每次成功转换还会清除过期的协调诊断，并递增 Version，供元数据
// 变更观察者识别新版本。
func (r *gormConversationRepository) Transition(ctx context.Context, appName, userID, conversationID string, from, to ConversationStatus) error {
	updates := map[string]any{
		"status":               to,
		"reconcile_attempts":   0,
		"last_reconcile_error": "",
		"updated_at":           time.Now(),
		"version":              gorm.Expr("version + 1"),
	}
	if to == ConversationDeleted {
		// 仅在进入 deleted 终态时写入墓碑时间，其他转换不能提前标记删除。
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

// ClaimAuto 获取或声明指定应用和用户的自动会话。
// 返回值依次为 (conversationID, claimed, error)：
//   - claimed=false 且 ID 非空：返回当前未过期的 active 会话；
//   - claimed=true：返回由 token 持有的固定候选 ID；
//   - claimed=false 且 ID 为空：其他创建者持有尚未过期的租约。
//
// 候选 ID 会在创建 ADK 会话前持久化，并在租约接管后保持不变，因此崩溃后每个
// 可能已提交的 ADK 会话仍然可寻址。条件更新提供比较并交换式隔离，无需在网络
// 调用期间长期持有数据库事务。
func (r *gormConversationRepository) ClaimAuto(ctx context.Context, appName, userID, token, candidateID string, now, staleBefore, leaseUntil time.Time) (string, bool, error) {
	var record autoConversation
	err := r.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ?", appName, userID).
		First(&record).Error
	// 当天仍活跃的指针可以直接复用；条件更新同时防止读取后被其他实例轮换。
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
	// 只有“记录不存在”允许继续尝试插入首次 claim，其他数据库错误必须立即返回。
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, fmt.Errorf("get auto conversation: %w", err)
	}

	// 将候选 ID 与 claim 一同持久化，使创建中途崩溃后保留可恢复指针，
	// 而不是留下无法追踪的孤儿 ADK 会话。
	claim := &autoConversation{
		AppName: appName, UserID: userID, ConversationID: candidateID,
		Status: autoConversationCreating, LeaseToken: token, LeaseUntil: leaseUntil, UpdatedAt: now,
	}
	result := r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(claim)
	if result.Error != nil {
		return "", false, fmt.Errorf("insert auto conversation claim: %w", result.Error)
	}
	if result.RowsAffected == 1 {
		// 首次插入成功，当前 token 获得候选 ID 的创建权。
		return candidateID, true, nil
	}

	// 插入未命中表示并发者已创建记录，重新读取后再判断等待、接管或轮换。
	if err = r.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ?", appName, userID).
		First(&record).Error; err != nil {
		return "", false, fmt.Errorf("get auto conversation claim: %w", err)
	}

	// 接管租约时保留已持久化的候选 ID，使每个可能已提交的创建仍可恢复。
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
			// 接管只替换 token 和租约，不替换候选 ID，避免丢失可能已提交的会话。
			return record.ConversationID, true, nil
		}
	}

	// 轮换已过期的 active 指针。旧会话属于保留的历史记录而非孤儿，因此不删除。
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
			// 跨自然日轮换使用全新候选 ID，旧 active 会话继续作为历史记录保留。
			return candidateID, true, nil
		}
	}

	// 当前调用者竞争失败；如果胜出者的指针已是未过期的 active，则直接复用。
	if err = r.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ?", appName, userID).
		First(&record).Error; err != nil {
		return "", false, fmt.Errorf("get auto conversation claim: %w", err)
	}
	if record.Status == autoConversationActive {
		// 并发者可能刚完成创建；再次以条件更新确认它仍是当天有效指针。
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

// CompleteAuto 仅在 ID 和 fencing token 均匹配时将 creating 候选推进为 active。
// 更新行数为零表示租约已被替换或操作已经完成；调用方必须重新加载状态，
// 不能覆盖竞争胜出者。
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
		// token、候选 ID 或状态任一不匹配，都说明当前调用者已失去完成资格。
		return errAutoConversationClaimLost
	}
	return nil
}

// ReleaseAuto 仅删除由 token 持有的 creating claim，用于结果明确的清理路径。
// ADK 创建结果未知时不得调用，否则删除持久化候选会使已提交会话无法追踪。
func (r *gormConversationRepository) ReleaseAuto(ctx context.Context, appName, userID, token string) error {
	result := r.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND status = ? AND lease_token = ?", appName, userID, autoConversationCreating, token).
		Delete(&autoConversation{})
	if result.Error != nil {
		return fmt.Errorf("release auto conversation claim: %w", result.Error)
	}
	return nil
}
