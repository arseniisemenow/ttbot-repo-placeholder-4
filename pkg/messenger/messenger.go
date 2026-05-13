// Package messenger is the Telegram abstraction used by identity-bot. The
// bot only operates in DMs and never needs forum topics, inline keyboards,
// pins or reactions — so the surface is much smaller than ttbot's.
package messenger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Messenger is the interface used by handlers.
type Messenger interface {
	SendMessage(ctx context.Context, chatID int64, text string) (int64, error)
	// SendMessageWithForceReply sends a message tagged with Telegram's
	// `force_reply` markup. The recipient's client shows an immediate reply
	// composer; their reply arrives as a Message with ReplyTo populated.
	// Used to split a sensitive prompt ("/new_read_key") from the actual
	// secret ("login:password") into two distinct messages so the bot can
	// delete only the secret.
	SendMessageWithForceReply(ctx context.Context, chatID int64, text, placeholder string) (int64, error)
	// DeleteMessage removes a message the bot or the user posted. Used to
	// scrub the user's S21-creds reply moments after we've read it.
	DeleteMessage(ctx context.Context, chatID, messageID int64) error
}

// Common error sentinels.
var (
	ErrForbidden = errors.New("messenger: forbidden")
	ErrNotFound  = errors.New("messenger: not found")
)

// Update is one inbound Telegram update (subset).
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message,omitempty"`
}

// Message is one Telegram message.
type Message struct {
	MessageID int64    `json:"message_id"`
	Chat      Chat     `json:"chat"`
	From      *User    `json:"from"`
	Text      string   `json:"text"`
	ReplyTo   *Message `json:"reply_to_message,omitempty"`
}

// Chat is a chat reference.
type Chat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Username string `json:"username"`
}

// User is a Telegram user.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// telegramAPI is the production Messenger.
type telegramAPI struct {
	token string
	http  *http.Client
}

// NewTelegram constructs a Messenger that speaks Telegram Bot API HTTP.
func NewTelegram(token string) Messenger {
	return &telegramAPI{token: token, http: &http.Client{Timeout: 10 * time.Second}}
}

const tgEndpoint = "https://api.telegram.org/bot"

func (t *telegramAPI) SendMessage(ctx context.Context, chatID int64, text string) (int64, error) {
	body, _ := json.Marshal(map[string]any{"chat_id": chatID, "text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tgEndpoint+t.token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("sendMessage: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	wrapper := struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code,omitempty"`
		Description string `json:"description,omitempty"`
		Result      struct {
			MessageID int64 `json:"message_id"`
		} `json:"result,omitempty"`
	}{}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return 0, fmt.Errorf("decode: %w body=%s", err, string(raw))
	}
	if !wrapper.OK {
		if wrapper.ErrorCode == 403 {
			return 0, fmt.Errorf("%w: %s", ErrForbidden, wrapper.Description)
		}
		if wrapper.ErrorCode == 400 || wrapper.ErrorCode == 404 {
			return 0, fmt.Errorf("%w: %s", ErrNotFound, wrapper.Description)
		}
		return 0, fmt.Errorf("telegram %d: %s", wrapper.ErrorCode, wrapper.Description)
	}
	return wrapper.Result.MessageID, nil
}

func (t *telegramAPI) SendMessageWithForceReply(ctx context.Context, chatID int64, text, placeholder string) (int64, error) {
	replyMarkup := map[string]any{
		"force_reply": true,
		"selective":   true,
	}
	if placeholder != "" {
		replyMarkup["input_field_placeholder"] = placeholder
	}
	body, _ := json.Marshal(map[string]any{
		"chat_id":      chatID,
		"text":         text,
		"reply_markup": replyMarkup,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tgEndpoint+t.token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("sendMessage: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	wrapper := struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code,omitempty"`
		Description string `json:"description,omitempty"`
		Result      struct {
			MessageID int64 `json:"message_id"`
		} `json:"result,omitempty"`
	}{}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return 0, fmt.Errorf("decode: %w body=%s", err, string(raw))
	}
	if !wrapper.OK {
		return 0, fmt.Errorf("telegram %d: %s", wrapper.ErrorCode, wrapper.Description)
	}
	return wrapper.Result.MessageID, nil
}

func (t *telegramAPI) DeleteMessage(ctx context.Context, chatID, messageID int64) error {
	body, _ := json.Marshal(map[string]any{"chat_id": chatID, "message_id": messageID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tgEndpoint+t.token+"/deleteMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		return fmt.Errorf("deleteMessage: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	wrapper := struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code,omitempty"`
		Description string `json:"description,omitempty"`
	}{}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return fmt.Errorf("decode: %w body=%s", err, string(raw))
	}
	if !wrapper.OK {
		if wrapper.ErrorCode == 400 || wrapper.ErrorCode == 404 {
			return fmt.Errorf("%w: %s", ErrNotFound, wrapper.Description)
		}
		return fmt.Errorf("telegram %d: %s", wrapper.ErrorCode, wrapper.Description)
	}
	return nil
}

// Mock is a test Messenger.
type Mock struct {
	mu    sync.Mutex
	Calls []MockCall
	Fail  error
}

// MockCall is one recorded send.
type MockCall struct {
	Method      string // "SendMessage" | "SendMessageWithForceReply" | "DeleteMessage"
	ChatID      int64
	Text        string
	Placeholder string
	MessageID   int64
}

// NewMock returns a Mock.
func NewMock() *Mock { return &Mock{} }

// SendMessage records the call.
func (m *Mock) SendMessage(_ context.Context, chatID int64, text string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		err := m.Fail
		m.Fail = nil
		return 0, err
	}
	m.Calls = append(m.Calls, MockCall{Method: "SendMessage", ChatID: chatID, Text: text})
	return int64(len(m.Calls)), nil
}

// SendMessageWithForceReply records the call.
func (m *Mock) SendMessageWithForceReply(_ context.Context, chatID int64, text, placeholder string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		err := m.Fail
		m.Fail = nil
		return 0, err
	}
	m.Calls = append(m.Calls, MockCall{Method: "SendMessageWithForceReply", ChatID: chatID, Text: text, Placeholder: placeholder})
	return int64(len(m.Calls)), nil
}

// DeleteMessage records the call.
func (m *Mock) DeleteMessage(_ context.Context, chatID, messageID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		err := m.Fail
		m.Fail = nil
		return err
	}
	m.Calls = append(m.Calls, MockCall{Method: "DeleteMessage", ChatID: chatID, MessageID: messageID})
	return nil
}

// LastText returns the most recent message text, or "" if none.
func (m *Mock) LastText() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.Calls) == 0 {
		return ""
	}
	return m.Calls[len(m.Calls)-1].Text
}
