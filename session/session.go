package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	ErrConversationNotFound = errors.New("conversation not found")
	ErrInvalidUserID        = errors.New("user id is required")
	// ErrAutoModeUnavailable 表示未通过 WithAutoMode 启用自动会话模式。
	ErrAutoModeUnavailable = errors.New("automatic conversations are not enabled")
)

const (
	sessionModeKey       = "__adk_session_mode"
	sessionModeExplicit  = "explicit"
	sessionModeAuto      = "auto"
	maxReconcileAttempts = 10
	autoClaimLease       = time.Minute
	autoClaimPoll        = 25 * time.Millisecond
)

type Session struct {
	name     string
	service  session.Service
	convRepo ConversationRepository
	autoMode bool

	reconcileEvery  time.Duration
	reconcileCancel context.CancelFunc
	reconcileDone   chan struct{}

	closeOnce sync.Once
}

func (s *Session) AppName() string {
	return s.name
}

func (s *Session) Service() session.Service {
	return s.service
}

func (s *Session) AutoModeEnabled() bool {
	return s.autoMode
}

func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		// 仅在后台任务实际启动时等待退出；禁用协调器时这两个字段均为空。
		if s.reconcileCancel != nil {
			s.reconcileCancel()
			<-s.reconcileDone
		}
	})
	return nil
}

// CreateConversation 在两个持久化存储中创建显式会话。
// 它先写入 creating 状态的元数据，再创建 ADK 会话，最后将元数据推进为 active。
// 这种顺序可以让 ReconcileConversations 发现并恢复每一种中断状态。
// conversationId 为空时，会在修改任一存储前生成 UUID。
func (s *Session) CreateConversation(ctx context.Context, userId, conversationId string) (session.Session, error) {
	if len(userId) == 0 {
		return nil, ErrInvalidUserID
	}
	if len(conversationId) == 0 {
		conversationId = uuid.NewString()
	}
	now := time.Now()
	metadata := &Conversation{
		AppName:        s.name,
		UserID:         userId,
		ConversationID: conversationId,
		Status:         ConversationCreating,
		Version:        1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.convRepo.Create(ctx, metadata); err != nil {
		return nil, err
	}

	resp, err := s.service.Create(ctx, &session.CreateRequest{
		AppName:   s.name,
		UserID:    userId,
		SessionID: conversationId,
		State: map[string]any{
			sessionModeKey: sessionModeExplicit,
		},
	})
	if err != nil {
		// 元数据已经落库，ADK 创建失败时必须补偿或保留为可协调状态。
		compensationErr := s.failCreatingConversation(ctx, userId, conversationId, err)
		return nil, errors.Join(err, compensationErr)
	}
	if err := s.convRepo.Transition(
		ctx, s.name, userId, conversationId, ConversationCreating, ConversationActive,
	); err != nil {
		// ADK 会话已创建成功，此处不能通过删除会话来回滚。
		// 遗留的 creating 记录将由协调器推进为 active。
		current, getErr := s.convRepo.Get(ctx, s.name, userId, conversationId)
		if getErr == nil && current.Status == ConversationActive {
			return resp.Session, nil
		}
		return nil, errors.Join(err, getErr)
	}
	return resp.Session, nil
}

// failCreatingConversation 对失败的 ADK 创建执行补偿。
// 如果清理失败，会有意保留 creating 状态，使协调器能够判断原创建操作是否已提交。
func (s *Session) failCreatingConversation(ctx context.Context, userId, conversationId string, createErr error) error {
	// 重复键表示该 ID 已被一个未纳入元数据管理的会话占用，不能删除它，
	// 只能将当前创建流程的元数据标记为失败。
	if !errors.Is(createErr, gorm.ErrDuplicatedKey) {
		if err := s.service.Delete(ctx, &session.DeleteRequest{
			AppName: s.name, UserID: userId, SessionID: conversationId,
		}); err != nil {
			// 保持 creating 状态，由协调器判断 Create 是否已经提交。
			return err
		}
	}
	return s.convRepo.Transition(
		ctx, s.name, userId, conversationId, ConversationCreating, ConversationFailed,
	)
}

func (s *Session) OwnsConversation(ctx context.Context, userId, conversationId string) error {
	return s.convRepo.Owns(ctx, s.name, userId, conversationId)
}

// TouchConversation 在一次条件更新中校验归属关系，并将会话移到用户列表前部。
func (s *Session) TouchConversation(ctx context.Context, userId, conversationId string) error {
	return s.convRepo.Touch(ctx, s.name, userId, conversationId, time.Now())
}

// GetConversation 仅在会话属于 userId 时返回会话及其事件。
func (s *Session) GetConversation(ctx context.Context, userId, conversationId string) (session.Session, error) {
	if err := s.OwnsConversation(ctx, userId, conversationId); err != nil {
		return nil, err
	}

	resp, err := s.service.Get(ctx, &session.GetRequest{
		AppName:   s.name,
		UserID:    userId,
		SessionID: conversationId,
	})
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Session == nil {
		return nil, ErrConversationNotFound
	}
	return resp.Session, nil
}

func (s *Session) ListConversations(ctx context.Context, userId, cursor string, limit int) (*ConversationPage, error) {
	return s.convRepo.List(ctx, s.name, userId, cursor, limit)
}

func hasSessionMode(sess session.Session, mode string) bool {
	value, err := sess.State().Get(sessionModeKey)
	return err == nil && value == mode
}

// DeleteConversation 跨两个存储执行可恢复删除。
// 删除 ADK 会话前，元数据会先进入 deleting，使会话立即对外隐藏；
// 对于中断或结果未知的删除，协调器会持续重试，直至元数据可标记为 deleted。
func (s *Session) DeleteConversation(ctx context.Context, userId, conversationId string) error {
	if err := s.convRepo.BeginDelete(ctx, s.name, userId, conversationId); err != nil {
		return err
	}
	if err := s.service.Delete(ctx, &session.DeleteRequest{
		AppName:   s.name,
		UserID:    userId,
		SessionID: conversationId,
	}); err != nil {
		// 删除可能在超时前已经提交，因此保持 deleting，供协调器幂等重试，
		// 避免重新暴露底层会话已经不存在的记录。
		return err
	}
	return s.convRepo.Transition(
		ctx, s.name, userId, conversationId, ConversationDeleting, ConversationDeleted,
	)
}

// GetOrCreate 返回 userId 当前的自动会话，适用于钉钉等不管理会话 ID 的渠道。
//
// ClaimAuto 按应用和用户串行化创建流程。候选 ID 会在创建 ADK 会话前随 claim
// 一同持久化，并且在租约接管时保持不变。因此进程在任意位置崩溃后，后续调用者
// 仍能重试或校验同一个 ADK 会话，不会产生无法追踪的孤儿会话。
// token 用作 fencing token，只有当前租约持有者才能将候选会话推进为 active。
func (s *Session) GetOrCreate(ctx context.Context, userId string) (string, error) {
	if !s.autoMode {
		return "", ErrAutoModeUnavailable
	}
	if len(userId) == 0 {
		return "", ErrInvalidUserID
	}
	for {
		now := time.Now()
		token := uuid.NewString()
		candidateId := uuid.NewString()
		conversationId, claimed, err := s.convRepo.ClaimAuto(
			ctx, s.name, userId, token, candidateId, now, startOfDay(now), now.Add(autoClaimLease),
		)
		if err != nil {
			return "", err
		}
		// 未获得 claim 且返回 ID，说明仓储已经找到可直接复用的 active 会话。
		if !claimed && len(conversationId) != 0 {
			return conversationId, nil
		}
		if !claimed {
			// 其他调用者持有尚未过期的创建租约。轮询无需长期占用数据库事务，
			// 同时仍可及时响应上下文取消。
			timer := time.NewTimer(autoClaimPoll)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctx.Err()
			case <-timer.C:
			}
			continue
		}

		// 获得 claim 后只能使用仓储返回的固定候选 ID；租约接管时它可能不同于
		// 本轮生成的 candidateId，但必须保持不变才能恢复可能已提交的会话。
		if _, createErr := s.service.Create(ctx, &session.CreateRequest{
			AppName:   s.name,
			UserID:    userId,
			SessionID: conversationId,
			State: map[string]any{
				sessionModeKey: sessionModeAuto,
			},
		}); createErr != nil {
			// Create 可能已经提交，但随后返回超时或传输错误。使用脱离原请求且
			// 带超时的上下文进行校验；结果未知时绝不能删除已持久化的候选记录。
			verifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			resp, getErr := s.service.Get(verifyCtx, &session.GetRequest{
				AppName: s.name, UserID: userId, SessionID: conversationId,
				NumRecentEvents: 1,
			})
			// 能按固定 ID 读到会话，证明 Create 实际已提交，只需完成元数据状态。
			if getErr == nil && resp != nil && resp.Session != nil {
				completeErr := s.completeAuto(verifyCtx, userId, token, conversationId)
				cancel()
				if completeErr == nil {
					return conversationId, nil
				}
				// 校验期间租约可能已被其他调用者接管，重新循环以读取胜出者状态。
				if errors.Is(completeErr, errAutoConversationClaimLost) && ctx.Err() == nil {
					continue
				}
				return "", errors.Join(createErr, completeErr)
			}
			cancel()
			// 保留已持久化的候选记录，待租约过期后继续恢复。
			return "", errors.Join(createErr, getErr)
		}

		// ADK 创建成功后即使原请求被取消，也要尽力完成元数据，避免等待整段租约。
		completeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = s.completeAuto(completeCtx, userId, token, conversationId)
		cancel()
		if err != nil {
			// token 不匹配表示其他调用者已经接管，当前调用者不能再写原 claim。
			if errors.Is(err, errAutoConversationClaimLost) {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				continue
			}
			return "", err
		}
		return conversationId, nil
	}
}

// completeAuto 在 ctx 有效期内重试临时性元数据故障。
// claim 丢失属于 fencing 决策而非存储故障，因此不会在此重试；
// 调用方必须重新加载竞争胜出者的状态。
func (s *Session) completeAuto(ctx context.Context, userId, token, conversationId string) error {
	for {
		err := s.convRepo.CompleteAuto(ctx, s.name, userId, token, conversationId, time.Now())
		if err == nil || errors.Is(err, errAutoConversationClaimLost) {
			return err
		}
		// 其余错误按临时存储故障处理，在限定时间内短间隔重试。
		timer := time.NewTimer(autoClaimPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}

// ReconcileConversations 恢复在元数据表与 ADK 会话服务之间中断的显式会话操作。
// 待处理记录按固定批量读取；仓储会推进失败记录的 UpdatedAt，避免单个持续故障
// 永久阻塞后续记录。
func (s *Session) ReconcileConversations(ctx context.Context) error {
	for {
		pending, err := s.convRepo.Pending(ctx, s.name, 100)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return nil
		}
		var reconcileErr error
		for _, conversation := range pending {
			if err := s.reconcileConversation(ctx, conversation); err != nil {
				// 单条失败不会中断本批次，避免故障记录饿死同批次后续记录。
				recordErr := s.convRepo.RecordReconcileFailure(
					ctx, s.name, conversation.UserID, conversation.ConversationID, conversation.Status,
					err.Error(), maxReconcileAttempts, time.Now(),
				)
				reconcileErr = errors.Join(reconcileErr, err, recordErr)
			}
		}
		if reconcileErr != nil {
			return reconcileErr
		}
	}
}

// reconcileConversation 以 ADK 存储为事实来源处理单条待定状态：
// creating 对应的会话存在时推进为 active，确认不存在时推进为 failed；
// deleting 状态则持续执行幂等删除。
func (s *Session) reconcileConversation(ctx context.Context, conversation Conversation) error {
	switch conversation.Status {
	case ConversationCreating:
		_, err := s.service.Get(ctx, &session.GetRequest{
			AppName: s.name, UserID: conversation.UserID, SessionID: conversation.ConversationID,
			NumRecentEvents: 1,
		})
		if err == nil {
			// 能读取 ADK 会话，说明跨存储创建已经越过提交点。
			return s.transitionOrAlready(ctx, conversation, ConversationCreating, ConversationActive)
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 只有明确的记录不存在才能终止创建；连接或数据库错误必须保留重试。
			return s.transitionOrAlready(ctx, conversation, ConversationCreating, ConversationFailed)
		}
		return fmt.Errorf("reconcile creating conversation %s: %w", conversation.ConversationID, err)
	case ConversationDeleting:
		if err := s.service.Delete(ctx, &session.DeleteRequest{
			AppName: s.name, UserID: conversation.UserID, SessionID: conversation.ConversationID,
		}); err != nil {
			return fmt.Errorf("reconcile deleting conversation %s: %w", conversation.ConversationID, err)
		}
		return s.transitionOrAlready(ctx, conversation, ConversationDeleting, ConversationDeleted)
	default:
		return nil
	}
}

// transitionOrAlready 在最新读取结果已是目标状态时，将并发完成或响应丢失的
// 状态转换视为成功。
func (s *Session) transitionOrAlready(ctx context.Context, conversation Conversation, from, to ConversationStatus) error {
	err := s.convRepo.Transition(
		ctx, s.name, conversation.UserID, conversation.ConversationID, from, to,
	)
	if !errors.Is(err, ErrConversationNotFound) {
		return err
	}
	current, getErr := s.convRepo.Get(ctx, s.name, conversation.UserID, conversation.ConversationID)
	if getErr == nil && current.Status == to {
		// 最新状态已经等于目标状态时，将并发完成或提交后响应丢失视为成功。
		return nil
	}
	return errors.Join(err, getErr)
}

// startReconciler 为当前 Session 启动一个可取消的协调任务。
// 随机抖动会在时间上分散多个应用实例发起的协调查询。
func (s *Session) startReconciler() {
	if s.reconcileEvery <= 0 {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.reconcileCancel = cancel
	s.reconcileDone = make(chan struct{})

	go func() {
		defer close(s.reconcileDone)

		timer := time.NewTimer(reconcileDelay(s.reconcileEvery))
		defer timer.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				if err := s.ReconcileConversations(ctx); err != nil && !errors.Is(err, context.Canceled) {
					slog.ErrorContext(ctx, "[session] reconcile conversations", slog.Any("error", err))
				}
				timer.Reset(reconcileDelay(s.reconcileEvery))
			}
		}
	}()
}

// reconcileDelay 在 base 基础上施加均匀分布的正负 20% 随机抖动。
func reconcileDelay(base time.Duration) time.Duration {
	spread := base / 5
	if spread <= 0 {
		return base
	}
	return base - spread + time.Duration(rand.Int64N(int64(2*spread)+1))
}

// startOfDay 返回服务器本地时区中 t 所在自然日的零点。
func startOfDay(t time.Time) time.Time {
	t = t.In(time.Local)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
}

// New 初始化 ADK 会话存储和会话元数据存储，执行一次尽力而为的协调，
// 并在未禁用时启动周期协调任务。调用方必须在停机时调用 Close 结束后台任务。
func New(name string, dialector gorm.Dialector, opts ...Option) (*Session, error) {
	svc, err := database.NewSessionService(dialector, &gorm.Config{
		TranslateError: true,
		Logger: logger.NewSlogLogger(slog.Default(), logger.Config{
			SlowThreshold:             time.Second,
			LogLevel:                  logger.Info,
			IgnoreRecordNotFoundError: true,
		}),
	})
	if err != nil {
		return nil, err
	}
	if err = database.AutoMigrate(svc); err != nil {
		return nil, err
	}

	convRepo, err := NewConversationRepository(dialector, &gorm.Config{
		TranslateError: true,
		Logger: logger.NewSlogLogger(slog.Default(), logger.Config{
			SlowThreshold:             time.Second,
			LogLevel:                  logger.Info,
			IgnoreRecordNotFoundError: true,
		}),
	})
	if err != nil {
		return nil, err
	}

	ctx := context.Background()

	s := &Session{
		name:           name,
		service:        svc,
		convRepo:       convRepo,
		reconcileEvery: time.Minute,
	}
	for _, opt := range opts {
		opt(s)
	}
	if err := s.ReconcileConversations(ctx); err != nil {
		slog.ErrorContext(ctx, "[session] initial conversation reconciliation", slog.Any("error", err))
	}
	s.startReconciler()

	return s, nil
}
