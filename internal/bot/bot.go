// Package bot adapts the existing Archilkilla bot's net/http long-polling and
// reply keyboard approach for LMS data. It is the only poller for its token.
package bot

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"artem-study/internal/app"
)

type Bot struct {
	App     *app.App
	Client  *http.Client
	BaseURL string
	mu      sync.Mutex
	dialogs map[int64]*dialog
	views   map[int64]*view
}
type Message struct {
	From *app.TelegramUser `json:"from"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Text string `json:"text"`
}
type Update struct {
	ID       int64     `json:"update_id"`
	Message  *Message  `json:"message"`
	Callback *Callback `json:"callback_query"`
}
type APIError struct {
	Code       int
	RetryAfter int
}

func (e *APIError) Error() string { return fmt.Sprintf("Telegram API status %d", e.Code) }
func New(a *app.App) *Bot {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if a.Config.IPv4 {
		d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		tr.DialContext = func(c context.Context, _ string, address string) (net.Conn, error) {
			return d.DialContext(c, "tcp4", address)
		}
	}
	return &Bot{App: a, Client: &http.Client{Timeout: 40 * time.Second, Transport: tr}, BaseURL: "https://api.telegram.org/bot" + a.Config.BotToken + "/"}
}
func (b *Bot) call(ctx context.Context, method string, payload any, out any) error {
	body, e := json.Marshal(payload)
	if e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, b.BaseURL+method, bytes.NewReader(body))
	if e != nil {
		return errors.New("invalid Telegram API URL")
	}
	req.Header.Set("Content-Type", "application/json")
	res, e := b.Client.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Telegram connection failed")
	}
	defer res.Body.Close()
	var envelope struct {
		OK         bool            `json:"ok"`
		Code       int             `json:"error_code"`
		Result     json.RawMessage `json:"result"`
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if e = json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&envelope); e != nil {
		return errors.New("invalid Telegram response")
	}
	if res.StatusCode != 200 || !envelope.OK {
		code := envelope.Code
		if code == 0 {
			code = res.StatusCode
		}
		return &APIError{Code: code, RetryAfter: envelope.Parameters.RetryAfter}
	}
	if out != nil {
		return json.Unmarshal(envelope.Result, out)
	}
	return nil
}
func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func (b *Bot) Run(ctx context.Context) error {
	if b.App.Config.BotToken == "" || b.App.Config.AdminTelegramID <= 0 {
		return errors.New("TELEGRAM_BOT_TOKEN and TELEGRAM_ADMIN_ID are required")
	}
	// Session-level lock stays on this dedicated connection for the poller's life.
	conn, e := b.App.DB.Conn(ctx)
	if e != nil {
		return e
	}
	defer conn.Close()
	var locked bool
	if e = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(8173402)").Scan(&locked); e != nil {
		return e
	}
	if !locked {
		return errors.New("another LMS bot is already running")
	}
	defer conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock(8173402)")
	var info struct {
		URL string `json:"url"`
	}
	if e = b.call(ctx, "getWebhookInfo", map[string]any{}, &info); e != nil {
		return e
	}
	if info.URL != "" {
		return errors.New("bot has an active webhook; stop the old integration and remove its webhook before starting polling")
	}
	if e = b.configureMenu(ctx); e != nil {
		log.Printf("bot menu setup: %v", e)
	}
	key := "offset:" + app.Hash(b.App.Config.BotToken)
	var offset int64
	e = b.App.DB.QueryRowContext(ctx, "SELECT value FROM bot_state WHERE key=$1", key).Scan(&offset)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	backoff := time.Second
	for ctx.Err() == nil {
		if e = conn.PingContext(ctx); e != nil {
			return errors.New("bot database lock connection lost")
		}
		var updates []Update
		e = b.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 25, "allowed_updates": []string{"message", "callback_query"}}, &updates)
		if e != nil {
			var ae *APIError
			if errors.As(e, &ae) && (ae.Code == 401 || ae.Code == 409) {
				return fmt.Errorf("%w; check token and stop the old poller", e)
			}
			delay := backoff
			if errors.As(e, &ae) && ae.RetryAfter > 0 {
				delay = time.Duration(ae.RetryAfter) * time.Second
			}
			log.Printf("bot polling: %v; retry in %s", e, delay)
			if !pause(ctx, delay) {
				break
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for _, u := range updates {
			if u.ID < offset {
				continue
			}
			if u.Message != nil {
				e = b.Handle(ctx, u.Message)
			} else if u.Callback != nil {
				e = b.HandleCallback(ctx, u.Callback)
			}
			if e != nil {
				log.Printf("bot handler: %v", e)
			}
			// Consume each update once: delivery failure must not repeat a write or advance a form twice.
			next := u.ID + 1
			if _, e = b.App.DB.ExecContext(ctx, "INSERT INTO bot_state(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value", key, next); e != nil {
				return e
			}
			offset = next
		}
	}
	return ctx.Err()
}
func splitText(s string) []string {
	r := []rune(s)
	out := []string{}
	for len(r) > 3500 {
		n := 3500
		for i := 3500; i > 2000; i-- {
			if r[i] == '\n' {
				n = i + 1
				break
			}
		}
		out = append(out, string(r[:n]))
		r = r[n:]
	}
	if len(r) > 0 {
		out = append(out, string(r))
	}
	return out
}
func (b *Bot) link(ctx context.Context, token string, tg int64) error {
	if len(token) != 64 {
		return errors.New("Некорректная ссылка. Получите новую в кабинете.")
	}
	tx, e := b.App.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var sid int64
	if e = tx.QueryRowContext(ctx, "SELECT student_id FROM telegram_invites WHERE token_hash=$1 AND expires_at>now()", app.Hash(token)).Scan(&sid); errors.Is(e, sql.ErrNoRows) {
		return errors.New("Ссылка истекла или уже использована. Получите новую в кабинете.")
	} else if e != nil {
		return e
	}
	var active bool
	if e = tx.QueryRowContext(ctx, "SELECT active FROM students WHERE id=$1 FOR UPDATE", sid).Scan(&active); e != nil {
		return e
	}
	if !active {
		return errors.New("Учетная запись находится в архиве.")
	}
	// Lock the student before consuming the invite, also serializing unlink/archive.
	if e = tx.QueryRowContext(ctx, "DELETE FROM telegram_invites WHERE token_hash=$1 AND expires_at>now() RETURNING student_id", app.Hash(token)).Scan(&sid); e != nil {
		return errors.New("Ссылка уже использована. Получите новую в кабинете.")
	}
	var old int64
	e = tx.QueryRowContext(ctx, "SELECT telegram_id FROM telegram_links WHERE student_id=$1", sid).Scan(&old)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if e == nil && old != tg {
		return errors.New("У ученика уже привязан другой Telegram. Сначала отключите его в кабинете.")
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO telegram_links(student_id,telegram_id) VALUES($1,$2) ON CONFLICT(student_id) DO NOTHING", sid, tg)
	if app.IsConflict(e) {
		return errors.New("Этот Telegram уже привязан к другому ученику.")
	}
	if e != nil {
		return e
	}
	return tx.Commit()
}
func lessonStatus(s string) string {
	return map[string]string{"planned": "запланировано", "completed": "проведено", "cancelled": "отменено", "missed": "пропуск"}[s]
}
func homeworkStatus(s string) string {
	return map[string]string{"assigned": "выдано", "submitted": "на проверке", "reviewed": "проверено"}[s]
}
