package llmchat

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"log/slog"

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

// Ask 用 userId 当前的自动会话发送一条消息，并返回这次运行所在的会话。
//
// 它面向钉钉这类不管理会话 ID 的渠道。凡是要把这次运行的信息持久化的调用方，
// 都必须记下返回的 ID，而不是之后重新解析一次：自动会话在午夜会轮换。
func (c *Chat) Ask(ctx context.Context, userId, text string) (string, iter.Seq2[*adk_session.Event, error], error) {
	conversationId, err := c.automatic(ctx, userId)
	if err != nil {
		return "", nil, err
	}
	return conversationId, c.run(ctx, userId, conversationId, genai.NewContentFromText(text, genai.RoleUser)), nil
}

// Confirm 恢复当前自动会话里的一次工具确认。
//
// conversationId 是发起该确认的会话，由 Ask 或 Reply 返回。这里是校验而不是
// 信任：自动会话在午夜轮换，第二天才点的按钮不能去恢复一次早已被放弃的执行。
func (c *Chat) Confirm(ctx context.Context, userId, conversationId, callId string, approved bool, payload any) (iter.Seq2[*adk_session.Event, error], error) {
	// 只解析一次当前会话，比较和恢复都基于它，避免日切瞬间「先比较后恢复」错位
	conversation, err := c.session.GetOrCreate(ctx, userId, sessionAllEvents)
	if err != nil {
		return nil, err
	}
	if conversation.ID() != conversationId {
		return nil, ErrConversationChanged
	}
	if err := resumableConfirmation(conversation, callId); err != nil {
		return nil, err
	}
	return c.confirm(ctx, userId, conversation.ID(), callId, approved, payload), nil
}

// Reply 恢复当前自动会话里因人工输入而暂停的图工作流，并返回这次运行所在的
// 会话。参见 Ask。
func (c *Chat) Reply(ctx context.Context, userId, interruptId string, payload any) (string, iter.Seq2[*adk_session.Event, error], error) {
	conversationId, err := c.automatic(ctx, userId)
	if err != nil {
		return "", nil, err
	}
	return conversationId, c.reply(ctx, userId, conversationId, interruptId, payload), nil
}

// Pending 是一个会话正在等待的全部事项。两类都会让运行暂停，但回答方式不同：
// 待答问题取用户的下一条消息，待确认调用取的是针对某次调用的同意/拒绝决定。
type Pending struct {
	Inputs        []*RequestInput
	Confirmations []*Confirmation
}

// Pending 返回当前自动会话正在等待的事项。
//
// 会话是唯一真相源，两类事项来自同一次历史加载。渠道侧对待处理事项的缓存会
// 过期、会失败，而暂停的运行在被回答之前一直是暂停的，所以调用方必须能够重建
// 这份列表，而不是信任自己对已发出卡片的记录。
func (c *Chat) Pending(ctx context.Context, userId string) (*Pending, error) {
	conversation, err := c.session.GetOrCreate(ctx, userId, sessionAllEvents)
	if err != nil {
		return nil, err
	}
	return pendingOf(conversation), nil
}

// PendingConversation 是显式会话版的 Pending。
func (c *Chat) PendingConversation(ctx context.Context, userId, conversationId string) (*Pending, error) {
	conversation, err := c.session.Get(ctx, userId, conversationId)
	if err != nil {
		return nil, err
	}
	return pendingOf(conversation), nil
}

func pendingOf(sess adk_session.Session) *Pending {
	return &Pending{
		Inputs:        pendingInputsOf(sess),
		Confirmations: pendingConfirmationsOf(sess),
	}
}

func pendingConfirmationsOf(sess adk_session.Session) []*Confirmation {
	requests, answered := confirmationsOf(sess)

	pending := make([]*Confirmation, 0, len(requests))
	for _, req := range requests {
		if !answered[req.CallId] {
			pending = append(pending, req)
		}
	}
	return pending
}

// pendingInputsOf 回放会话历史，按提出顺序返回还没有对应回答的输入请求。
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
// 自动会话跨自然日轮换，ResetAutomatic 也会把指针换到全新的 ID，两种情况下旧
// 卡片指向的那次执行都已被放弃。恢复它只会得到一个语焉不详的「找不到 callId」，
// 所以在这里统一拦下。
var ErrConversationChanged = errors.New("llmchat: conversation changed")

// ErrConfirmationNotFound 表示当前会话里没有仍待处理的指定确认。会话既然匹配，
// 正常流程不会走到这里，它是防御性兜底：调用方传错 callId、或会话历史意外缺失
// 时，绝不能把一个查无来源的确认发给 runner。
var ErrConfirmationNotFound = errors.New("llmchat: tool confirmation not found")

// resumableConfirmation 判断 callId 是否还能被恢复。
//
// 已答优先于未找到：重复点击是常态，报「已处理」比报「不存在」更贴近事实。
func resumableConfirmation(sess adk_session.Session, callId string) error {
	requests, answered := confirmationsOf(sess)
	if answered[callId] {
		return ErrAlreadyConfirmed
	}
	for _, req := range requests {
		if req.CallId == callId {
			return nil
		}
	}
	return ErrConfirmationNotFound
}

// confirmationsOf 回放一遍会话历史，返回按提出顺序排列的工具确认请求，以及已经
// 给出过决定的 callId。
func confirmationsOf(sess adk_session.Session) ([]*Confirmation, map[string]bool) {
	events := sess.Events()

	requests := make([]*Confirmation, 0)
	answered := make(map[string]bool)
	seen := make(map[string]bool)

	for i := range events.Len() {
		event := events.At(i)
		if confirm, ok := ConfirmationOf(event); ok && !seen[confirm.CallId] {
			seen[confirm.CallId] = true
			requests = append(requests, confirm)
		}
		if event == nil || event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if fr := part.FunctionResponse; fr != nil && fr.Name == toolconfirmation.FunctionCallName {
				answered[fr.ID] = true
			}
		}
	}
	return requests, answered
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

// ResetAutomatic 丢弃当前的自动会话，包括因人工输入而暂停的图工作流。
// 下一次 Ask 会创建一个干净的会话。
func (c *Chat) ResetAutomatic(ctx context.Context, userId string) error {
	return c.session.ResetAutomatic(ctx, userId)
}

// NewConversation 创建一个新会话并返回生成的 ID。
func (c *Chat) NewConversation(ctx context.Context, userId string) (string, error) {
	conversation, err := c.session.Create(ctx, userId, "")
	if err != nil {
		return "", err
	}
	return conversation.ID(), nil
}

// CreateConversation 用调用方指定的 ID 创建会话。
func (c *Chat) CreateConversation(ctx context.Context, userId, conversationId string) error {
	_, err := c.session.Create(ctx, userId, conversationId)
	return err
}

func (c *Chat) GetConversation(ctx context.Context, userId, conversationId string) (adk_session.Session, error) {
	return c.session.Get(ctx, userId, conversationId)
}

// GetConversationMeta 返回产品层的元数据，包含标题。
func (c *Chat) GetConversationMeta(ctx context.Context, userId, conversationId string) (*session.Conversation, error) {
	return c.session.GetMeta(ctx, userId, conversationId)
}

// RenameConversation 设置已有会话的展示标题。
func (c *Chat) RenameConversation(ctx context.Context, userId, conversationId, title string) error {
	return c.session.Rename(ctx, userId, conversationId, title)
}

func (c *Chat) ListConversations(ctx context.Context, userId, cursor string, limit int) (*session.ConversationPage, error) {
	return c.session.List(ctx, userId, cursor, limit)
}

func (c *Chat) DeleteConversation(ctx context.Context, userId, conversationId string) error {
	return c.session.Delete(ctx, userId, conversationId)
}

// AskConversation 向一个已存在的显式会话发送消息。
func (c *Chat) AskConversation(ctx context.Context, userId, conversationId, text string) (iter.Seq2[*adk_session.Event, error], error) {
	if err := c.session.Touch(ctx, userId, conversationId); err != nil {
		return nil, err
	}
	return c.run(ctx, userId, conversationId, genai.NewContentFromText(text, genai.RoleUser)), nil
}

// ConfirmConversation 恢复显式会话里因工具确认而暂停的运行。
// userId 和 conversationId 必须与最初 Ask 时用的一致，callId 是 ConfirmationOf
// 给出的 Confirmation.CallId。payload 是转发给工具的应用自定义值，可为 nil。
func (c *Chat) ConfirmConversation(ctx context.Context, userId, conversationId, callId string, approved bool, payload any) (iter.Seq2[*adk_session.Event, error], error) {
	if err := c.session.Touch(ctx, userId, conversationId); err != nil {
		return nil, err
	}

	conversation, err := c.session.Get(ctx, userId, conversationId)
	if err != nil {
		return nil, err
	}
	if err := resumableConfirmation(conversation, callId); err != nil {
		return nil, err
	}
	return c.confirm(ctx, userId, conversationId, callId, approved, payload), nil
}

// ReplyConversation 恢复显式会话里因人工输入而暂停的图工作流。interruptId 是
// RequestInputOf 给出的 RequestInput.InterruptId，payload 是用户的回答，会作为
// 输入转发给该节点的后继节点。
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

	slog.Info("[llmchat] initialized")

	return &Chat{
		runner:  r,
		session: session,
	}, nil
}
