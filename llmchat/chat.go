package llmchat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"

	"github.com/noble-gase/argon/session"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	adk_session "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

type Chat struct {
	runner  *runner.Runner
	session *session.Session
}

const (
	// sessionIdOnly 只需要会话 ID 时的事件加载数，避免白取历史。
	sessionIdOnly = 1

	// sessionAllEvents 全量加载：重建待回答问题必须看到整段历史，
	// 只取最近若干条会漏掉更早发出、仍未回答的请求。
	sessionAllEvents = 0
)

func (c *Chat) Name() string {
	return c.session.AppName()
}

// automatic 返回 userId 的自动会话 ID，供不管理会话 ID 的渠道使用。
func (c *Chat) automatic(ctx context.Context, userId string) (string, error) {
	conversation, err := c.session.GetOrCreate(ctx, userId, sessionIdOnly)
	if err != nil {
		return "", err
	}
	return conversation.ID(), nil
}

// Ask sends a message using the current automatic conversation for userId, and
// returns the conversation it ran in.
//
// It is intended for channels such as DingTalk that do not manage conversation IDs.
// Callers that persist anything about this run must record the returned ID rather
// than resolving it again later: the automatic conversation rotates at midnight.
func (c *Chat) Ask(ctx context.Context, userId, text string) (string, iter.Seq2[*adk_session.Event, error], error) {
	conversationId, err := c.automatic(ctx, userId)
	if err != nil {
		return "", nil, err
	}
	return conversationId, c.run(ctx, userId, conversationId, genai.NewContentFromText(text, genai.RoleUser)), nil
}

// Confirm resumes a confirmation in the current automatic conversation.
//
// conversationId is the conversation that raised the confirmation, as returned by
// Ask or Reply. It is checked rather than trusted: the automatic conversation
// rotates at midnight, and a button tapped the next day must not resume a run
// that has already been left behind.
func (c *Chat) Confirm(ctx context.Context, userId, conversationId, callId string, approved bool, payload any) (iter.Seq2[*adk_session.Event, error], error) {
	// 只解析一次当前会话，比较和恢复都基于它，避免日切瞬间「先比较后恢复」错位
	conversation, err := c.session.GetOrCreate(ctx, userId, sessionAllEvents)
	if err != nil {
		return nil, err
	}
	if conversation.ID() != conversationId {
		return nil, ErrConversationChanged
	}
	if answeredConfirmations(conversation)[callId] {
		return nil, ErrAlreadyConfirmed
	}
	return c.confirm(ctx, userId, conversation.ID(), callId, approved, payload), nil
}

// Reply resumes a graph workflow paused on a human-input request in the current
// automatic conversation, and returns the conversation it ran in. See Ask.
func (c *Chat) Reply(ctx context.Context, userId, interruptId string, payload any) (string, iter.Seq2[*adk_session.Event, error], error) {
	conversationId, err := c.automatic(ctx, userId)
	if err != nil {
		return "", nil, err
	}
	return conversationId, c.reply(ctx, userId, conversationId, interruptId, payload), nil
}

// PendingInputs returns the human-input requests of the current automatic
// conversation that are still waiting for an answer.
//
// The session is the source of truth. A channel-side cache of pending questions
// can expire or fail, but a graph node stays parked until its request is
// answered, so callers must be able to rebuild the list from history.
func (c *Chat) PendingInputs(ctx context.Context, userId string) ([]*RequestInput, error) {
	conversation, err := c.session.GetOrCreate(ctx, userId, sessionAllEvents)
	if err != nil {
		return nil, err
	}
	return pendingInputsOf(conversation), nil
}

// PendingInputsConversation is PendingInputs for an explicit conversation.
func (c *Chat) PendingInputsConversation(ctx context.Context, userId, conversationId string) ([]*RequestInput, error) {
	conversation, err := c.session.Get(ctx, userId, conversationId)
	if err != nil {
		return nil, err
	}
	return pendingInputsOf(conversation), nil
}

// pendingInputsOf replays the session history and returns, in the order they
// were raised, the input requests that have no matching response yet.
func pendingInputsOf(sess adk_session.Session) []*RequestInput {
	events := sess.Events()

	requests := make(map[string]*RequestInput)
	replies := make(map[string]any)
	order := make([]string, 0)

	for i := range events.Len() {
		event := events.At(i)
		if event == nil {
			continue
		}

		if in, ok := RequestInputOf(event); ok && in.InterruptId != "" {
			if _, dup := requests[in.InterruptId]; !dup {
				order = append(order, in.InterruptId)
			}
			requests[in.InterruptId] = in
		}

		if event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			// 同一个请求可能被答多次，以最后一次为准，与 ADK 的 last-wins 一致
			if fr := part.FunctionResponse; fr != nil && fr.Name == workflow.WorkflowInputFunctionCallName {
				replies[fr.ID] = decodeReply(fr)
			}
		}
	}

	pending := make([]*RequestInput, 0, len(order))
	for _, id := range order {
		reply, replied := replies[id]
		if replied && acceptedReply(requests[id], reply) {
			continue
		}
		pending = append(pending, requests[id])
	}
	return pending
}

// acceptedReply 判断这次回答是否真的解决了请求。
//
// 光看「有没有 FunctionResponse」不够：不符合 ResponseSchema 的回答会被 ADK 拒绝
// （ErrInvalidResumeResponse），节点仍然 parked。若把它当成已回答，问题就会从待答
// 列表里消失，用户再也没机会重答。
func acceptedReply(req *RequestInput, reply any) bool {
	if req == nil || req.ResponseSchema == nil {
		return true
	}

	resolved, err := req.ResponseSchema.Resolve(nil)
	if err != nil {
		// schema 本身不可用时无从校验，按接受处理，避免永远重复追问
		return true
	}
	return resolved.Validate(reply) == nil
}

// ErrAlreadyConfirmed 表示这次工具确认已经做过决定。带副作用的工具绝不能因为
// 用户重复点击而执行两次，所以判断依据是会话历史，而不是渠道侧的缓存状态。
var ErrAlreadyConfirmed = errors.New("llmchat: tool confirmation already answered")

// ErrConversationChanged 表示这次确认所属的会话已经不是当前自动会话。
//
// 自动会话按自然日轮换，跨日后旧卡片指向的那次执行已被放弃，恢复它只会得到一个
// 语焉不详的「找不到 callId」。注意这只能识别轮换：当天 ResetAutomatic 会重建出
// 同一个确定性 ID，那种情况要靠渠道侧作废旧卡片。
var ErrConversationChanged = errors.New("llmchat: conversation changed")

// answeredConfirmations 回放会话历史，返回已经给出过决定的工具确认 callId。
func answeredConfirmations(sess adk_session.Session) map[string]bool {
	events := sess.Events()

	answered := make(map[string]bool)
	for i := range events.Len() {
		event := events.At(i)
		if event == nil || event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if fr := part.FunctionResponse; fr != nil && fr.Name == toolconfirmation.FunctionCallName {
				answered[fr.ID] = true
			}
		}
	}
	return answered
}

// decodeReply 取出 FunctionResponse 携带的回答，与 ADK 解析 resume 响应的规则一致。
func decodeReply(fr *genai.FunctionResponse) any {
	if raw, ok := fr.Response["response"]; ok {
		if s, isStr := raw.(string); isStr {
			var decoded any
			if err := json.Unmarshal([]byte(s), &decoded); err == nil {
				return decoded
			}
			return s
		}
		return raw
	}
	if payload, ok := fr.Response["payload"]; ok {
		return payload
	}
	return fr.Response
}

// ResetAutomatic abandons the current automatic conversation, including any
// graph workflow paused for human input. The next Ask creates a clean session.
func (c *Chat) ResetAutomatic(ctx context.Context, userId string) error {
	return c.session.ResetAutomatic(ctx, userId)
}

// NewConversation creates a new conversation and returns its generated ID.
func (c *Chat) NewConversation(ctx context.Context, userId string) (string, error) {
	conversation, err := c.session.Create(ctx, userId, "")
	if err != nil {
		return "", err
	}
	return conversation.ID(), nil
}

// CreateConversation creates a conversation with a caller-provided ID.
func (c *Chat) CreateConversation(ctx context.Context, userId, conversationId string) error {
	_, err := c.session.Create(ctx, userId, conversationId)
	return err
}

func (c *Chat) GetConversation(ctx context.Context, userId, conversationId string) (adk_session.Session, error) {
	return c.session.Get(ctx, userId, conversationId)
}

// GetConversationMeta returns the product-level metadata, including the title.
func (c *Chat) GetConversationMeta(ctx context.Context, userId, conversationId string) (*session.Conversation, error) {
	return c.session.GetMeta(ctx, userId, conversationId)
}

// RenameConversation sets the display title of an existing conversation.
func (c *Chat) RenameConversation(ctx context.Context, userId, conversationId, title string) error {
	return c.session.Rename(ctx, userId, conversationId, title)
}

func (c *Chat) ListConversations(ctx context.Context, userId, cursor string, limit int) (*session.ConversationPage, error) {
	return c.session.List(ctx, userId, cursor, limit)
}

func (c *Chat) DeleteConversation(ctx context.Context, userId, conversationId string) error {
	return c.session.Delete(ctx, userId, conversationId)
}

// AskConversation sends a message to an existing explicit conversation.
func (c *Chat) AskConversation(ctx context.Context, userId, conversationId, text string) (iter.Seq2[*adk_session.Event, error], error) {
	if err := c.session.Touch(ctx, userId, conversationId); err != nil {
		return nil, err
	}
	return c.run(ctx, userId, conversationId, genai.NewContentFromText(text, genai.RoleUser)), nil
}

// ConfirmConversation resumes a paused tool-confirmation run in an explicit conversation.
// userId and conversationId must be the values used in the original Ask call, and
// callId is the Confirmation.CallID surfaced by ConfirmationOf. payload is an
// optional application-specific value forwarded to the tool (may be nil).
func (c *Chat) ConfirmConversation(ctx context.Context, userId, conversationId, callId string, approved bool, payload any) (iter.Seq2[*adk_session.Event, error], error) {
	if err := c.session.Touch(ctx, userId, conversationId); err != nil {
		return nil, err
	}

	conversation, err := c.session.Get(ctx, userId, conversationId)
	if err != nil {
		return nil, err
	}
	if answeredConfirmations(conversation)[callId] {
		return nil, ErrAlreadyConfirmed
	}
	return c.confirm(ctx, userId, conversationId, callId, approved, payload), nil
}

// ReplyConversation resumes a graph workflow paused on a human-input request in
// an explicit conversation. interruptId is the RequestInput.InterruptId surfaced
// by RequestInputOf, and payload is the user's answer forwarded to the node's
// successor as its input.
func (c *Chat) ReplyConversation(ctx context.Context, userId, conversationId, interruptId string, payload any) (iter.Seq2[*adk_session.Event, error], error) {
	if err := c.session.Touch(ctx, userId, conversationId); err != nil {
		return nil, err
	}
	return c.reply(ctx, userId, conversationId, interruptId, payload), nil
}

func (c *Chat) run(ctx context.Context, userId, sessionId string, content *genai.Content) iter.Seq2[*adk_session.Event, error] {
	return c.runner.Run(
		ctx, userId, sessionId, content,
		agent.RunConfig{
			StreamingMode: agent.StreamingModeSSE,
		},
	)
}

func (c *Chat) confirm(ctx context.Context, userId, sessionId, callId string, approved bool, payload any) iter.Seq2[*adk_session.Event, error] {
	response := map[string]any{
		"confirmed": approved,
	}
	if payload != nil {
		response["payload"] = payload
	}

	content := &genai.Content{
		Role: string(genai.RoleUser),
		Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				Name:     toolconfirmation.FunctionCallName,
				ID:       callId,
				Response: response,
			},
		}},
	}

	return c.run(ctx, userId, sessionId, content)
}

func (c *Chat) reply(ctx context.Context, userId, sessionId, interruptId string, payload any) iter.Seq2[*adk_session.Event, error] {
	content := &genai.Content{
		Role: string(genai.RoleUser),
		Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				Name: workflow.WorkflowInputFunctionCallName,
				ID:   interruptId,
				// payload 键让 ADK 原样透传，不会对字符串再做一次 JSON 解析
				Response: map[string]any{"payload": payload},
			},
		}},
	}

	return c.run(ctx, userId, sessionId, content)
}

func NewChat(agent agent.Agent, session *session.Session) (*Chat, error) {
	// Runner
	r, err := runner.New(runner.Config{
		AppName:        session.AppName(),
		Agent:          agent,
		SessionService: session.Service(),
	})
	if err != nil {
		return nil, err
	}

	fmt.Println("ADK llmchat success")

	return &Chat{
		runner:  r,
		session: session,
	}, nil
}
