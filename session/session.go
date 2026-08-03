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
	// ErrAutoModeUnavailable is returned by GetOrCreate when automatic
	// conversations are not enabled via WithAutoMode.
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
		if s.reconcileCancel != nil {
			s.reconcileCancel()
			<-s.reconcileDone
		}
	})
	return nil
}

// CreateConversation creates an explicit conversation for a user. When
// conversationId is empty, the session service generates one.
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
		compensationErr := s.failCreatingConversation(ctx, userId, conversationId, err)
		return nil, errors.Join(err, compensationErr)
	}
	if err := s.convRepo.Transition(
		ctx, s.name, userId, conversationId, ConversationCreating, ConversationActive,
	); err != nil {
		// The session was created successfully; never roll back by deleting it.
		// A left-behind creating row is promoted to active by the reconciler.
		current, getErr := s.convRepo.Get(ctx, s.name, userId, conversationId)
		if getErr == nil && current.Status == ConversationActive {
			return resp.Session, nil
		}
		return nil, errors.Join(err, getErr)
	}
	return resp.Session, nil
}

func (s *Session) failCreatingConversation(ctx context.Context, userId, conversationId string, createErr error) error {
	// A duplicate key means an untracked session already owns this id; never
	// delete it, only mark our own metadata failed.
	if !errors.Is(createErr, gorm.ErrDuplicatedKey) {
		if err := s.service.Delete(ctx, &session.DeleteRequest{
			AppName: s.name, UserID: userId, SessionID: conversationId,
		}); err != nil {
			// Keep creating so reconciliation can determine whether Create committed.
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

// TouchConversation verifies ownership and moves the thread to the front of
// the user's list in one conditional update.
func (s *Session) TouchConversation(ctx context.Context, userId, conversationId string) error {
	return s.convRepo.Touch(ctx, s.name, userId, conversationId, time.Now())
}

// GetConversation returns a conversation with its events, only when it belongs
// to userId.
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

// DeleteConversation deletes a conversation after verifying its ownership.
func (s *Session) DeleteConversation(ctx context.Context, userId, conversationId string) error {
	if err := s.convRepo.BeginDelete(ctx, s.name, userId, conversationId); err != nil {
		return err
	}
	if err := s.service.Delete(ctx, &session.DeleteRequest{
		AppName:   s.name,
		UserID:    userId,
		SessionID: conversationId,
	}); err != nil {
		// The delete may have committed before a timeout. Keep deleting so the
		// reconciler can retry idempotently instead of exposing a missing session.
		return err
	}
	return s.convRepo.Transition(
		ctx, s.name, userId, conversationId, ConversationDeleting, ConversationDeleted,
	)
}

// GetOrCreate returns the current automatic conversation for userId, backed by
// the conversation metadata store. It is intended for channels such as DingTalk
// that do not manage conversation IDs.
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
		if !claimed && len(conversationId) != 0 {
			return conversationId, nil
		}
		if !claimed {
			timer := time.NewTimer(autoClaimPoll)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctx.Err()
			case <-timer.C:
			}
			continue
		}

		if _, createErr := s.service.Create(ctx, &session.CreateRequest{
			AppName:   s.name,
			UserID:    userId,
			SessionID: conversationId,
			State: map[string]any{
				sessionModeKey: sessionModeAuto,
			},
		}); createErr != nil {
			verifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			resp, getErr := s.service.Get(verifyCtx, &session.GetRequest{
				AppName: s.name, UserID: userId, SessionID: conversationId,
				NumRecentEvents: 1,
			})
			if getErr == nil && resp != nil && resp.Session != nil {
				completeErr := s.completeAuto(verifyCtx, userId, token, conversationId)
				cancel()
				if completeErr == nil {
					return conversationId, nil
				}
				if errors.Is(completeErr, errAutoConversationClaimLost) && ctx.Err() == nil {
					continue
				}
				return "", errors.Join(createErr, completeErr)
			}
			cancel()
			// Keep the persisted candidate for recovery after the lease expires.
			return "", errors.Join(createErr, getErr)
		}
		completeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = s.completeAuto(completeCtx, userId, token, conversationId)
		cancel()
		if err != nil {
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

func (s *Session) completeAuto(ctx context.Context, userId, token, conversationId string) error {
	for {
		err := s.convRepo.CompleteAuto(ctx, s.name, userId, token, conversationId, time.Now())
		if err == nil || errors.Is(err, errAutoConversationClaimLost) {
			return err
		}
		timer := time.NewTimer(autoClaimPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}

// ReconcileConversations completes metadata operations interrupted between the
// conversation table and the ADK session service.
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

func (s *Session) reconcileConversation(ctx context.Context, conversation Conversation) error {
	switch conversation.Status {
	case ConversationCreating:
		_, err := s.service.Get(ctx, &session.GetRequest{
			AppName: s.name, UserID: conversation.UserID, SessionID: conversation.ConversationID,
			NumRecentEvents: 1,
		})
		if err == nil {
			return s.transitionOrAlready(ctx, conversation, ConversationCreating, ConversationActive)
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
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

func (s *Session) transitionOrAlready(ctx context.Context, conversation Conversation, from, to ConversationStatus) error {
	err := s.convRepo.Transition(
		ctx, s.name, conversation.UserID, conversation.ConversationID, from, to,
	)
	if !errors.Is(err, ErrConversationNotFound) {
		return err
	}
	current, getErr := s.convRepo.Get(ctx, s.name, conversation.UserID, conversation.ConversationID)
	if getErr == nil && current.Status == to {
		return nil
	}
	return errors.Join(err, getErr)
}

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

func reconcileDelay(base time.Duration) time.Duration {
	spread := base / 5
	if spread <= 0 {
		return base
	}
	return base - spread + time.Duration(rand.Int64N(int64(2*spread)+1))
}

// startOfDay returns midnight of t's calendar day in the server's local zone.
func startOfDay(t time.Time) time.Time {
	t = t.In(time.Local)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
}

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
