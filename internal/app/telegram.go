package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const LessonAddress = "Лесная 16, 10 подъезд, 12 этаж, квартира 666, код от домофона ключ123321ключ"

var ErrTelegramAccount = errors.New("Кабинет не найден или недоступен. Попросите преподавателя создать ученика с логином, совпадающим с вашим Telegram username без @. Если аккаунт уже привязан, преподаватель должен сначала отключить прежнюю привязку.")

type TelegramUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	IsBot    bool   `json:"is_bot"`
}

// VerifyMiniApp accepts only Telegram-signed, fresh data. Never trust initDataUnsafe.
func VerifyMiniApp(raw, token string, now time.Time) (TelegramUser, error) {
	var user TelegramUser
	if token == "" || len(raw) > 16384 {
		return user, errors.New("invalid Telegram data")
	}
	q, e := url.ParseQuery(raw)
	if e != nil {
		return user, e
	}
	for _, v := range q {
		if len(v) != 1 {
			return user, errors.New("duplicate Telegram field")
		}
	}
	supplied, e := hex.DecodeString(q.Get("hash"))
	if e != nil || len(supplied) != 32 {
		return user, errors.New("invalid signature")
	}
	q.Del("hash")
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+"="+q.Get(k))
	}
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(token))
	mac := hmac.New(sha256.New, secret.Sum(nil))
	mac.Write([]byte(strings.Join(lines, "\n")))
	if !hmac.Equal(supplied, mac.Sum(nil)) {
		return user, errors.New("invalid signature")
	}
	epoch, e := strconv.ParseInt(q.Get("auth_date"), 10, 64)
	if e != nil || epoch < now.Add(-5*time.Minute).Unix() || epoch > now.Add(30*time.Second).Unix() {
		return user, errors.New("expired Telegram data")
	}
	if json.Unmarshal([]byte(q.Get("user")), &user) != nil || user.ID <= 0 || user.IsBot {
		return TelegramUser{}, errors.New("invalid Telegram user")
	}
	return user, nil
}

// TelegramAccount is called only after validating initData or receiving an authenticated Bot API update.
// Username is used once for discovery. A permanent numeric ID pin survives rename and archive.
func (a *App) TelegramAccount(ctx context.Context, tg TelegramUser) (User, error) {
	var u User
	if tg.ID <= 0 || tg.IsBot {
		return u, ErrTelegramAccount
	}
	if a.Config.AdminTelegramID > 0 && tg.ID == a.Config.AdminTelegramID {
		e := a.DB.QueryRowContext(ctx, "SELECT id,login,role,0 FROM users WHERE role='admin' AND login=$1", strings.ToLower(a.Config.AdminLogin)).Scan(&u.ID, &u.Login, &u.Role, &u.StudentID)
		if errors.Is(e, sql.ErrNoRows) {
			return u, ErrTelegramAccount
		}
		return u, e
	}
	tx, e := a.DB.BeginTx(ctx, nil)
	if e != nil {
		return u, e
	}
	defer tx.Rollback()
	var active bool
	e = tx.QueryRowContext(ctx, `SELECT u.id,u.login,u.role,u.student_id,s.active FROM telegram_identities t JOIN users u ON u.id=t.user_id JOIN students s ON s.id=u.student_id WHERE t.telegram_id=$1 FOR UPDATE OF s`, tg.ID).Scan(&u.ID, &u.Login, &u.Role, &u.StudentID, &active)
	if e == nil {
		if !active {
			return User{}, ErrTelegramAccount
		}
		return u, tx.Commit()
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return u, e
	}
	// Preserve existing invite-based links before trying username discovery.
	e = tx.QueryRowContext(ctx, `SELECT u.id,u.login,u.role,u.student_id,s.active FROM telegram_links t JOIN users u ON u.student_id=t.student_id JOIN students s ON s.id=u.student_id WHERE t.telegram_id=$1 FOR UPDATE OF s`, tg.ID).Scan(&u.ID, &u.Login, &u.Role, &u.StudentID, &active)
	if errors.Is(e, sql.ErrNoRows) {
		name := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(tg.Username), "@"))
		if name == "" {
			return u, ErrTelegramAccount
		}
		var matches int
		if e = tx.QueryRowContext(ctx, "SELECT count(*) FROM users WHERE role='student' AND lower(ltrim(login,'@'))=$1", name).Scan(&matches); e != nil {
			return u, e
		}
		if matches != 1 {
			return u, ErrTelegramAccount
		}
		e = tx.QueryRowContext(ctx, `SELECT u.id,u.login,u.role,u.student_id,s.active FROM users u JOIN students s ON s.id=u.student_id WHERE u.role='student' AND lower(ltrim(u.login,'@'))=$1 FOR UPDATE OF s`, name).Scan(&u.ID, &u.Login, &u.Role, &u.StudentID, &active)
	}
	if errors.Is(e, sql.ErrNoRows) {
		return u, ErrTelegramAccount
	}
	if e != nil {
		return u, e
	}
	if !active {
		return User{}, ErrTelegramAccount
	}
	// Do not silently steal a previously linked student, including archived/re-enabled accounts.
	var count int
	e = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM telegram_links WHERE student_id=$1 AND telegram_id<>$2)+(SELECT count(*) FROM telegram_identities WHERE user_id=$3 AND telegram_id<>$2)`, u.StudentID, tg.ID, u.ID).Scan(&count)
	if e != nil {
		return u, e
	}
	if count != 0 {
		return User{}, ErrTelegramAccount
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO telegram_identities(telegram_id,user_id) VALUES($1,$2)", tg.ID, u.ID); e != nil {
		if IsConflict(e) {
			return User{}, ErrTelegramAccount
		}
		return u, e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO telegram_links(student_id,telegram_id) VALUES($1,$2) ON CONFLICT(student_id) DO NOTHING", u.StudentID, tg.ID); e != nil {
		if IsConflict(e) {
			return User{}, ErrTelegramAccount
		}
		return u, e
	}
	return u, tx.Commit()
}

func sessionToken(r *http.Request) string {
	if v := r.Header.Get("X-Mini-App-Session"); v != "" {
		return v
	}
	c, e := r.Cookie("artem_session")
	if e != nil {
		return ""
	}
	return c.Value
}
func (api *API) telegramLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		InitData string `json:"init_data"`
	}
	if !decode(w, r, &in) {
		return
	}
	tg, e := VerifyMiniApp(in.InitData, api.Config.BotToken, time.Now())
	if e != nil {
		fail(w, 401, "Закройте приложение и снова откройте его кнопкой в Telegram.")
		return
	}
	u, e := api.TelegramAccount(r.Context(), tg)
	if errors.Is(e, ErrTelegramAccount) {
		fail(w, 403, e.Error())
		return
	}
	if e != nil {
		dbFail(w, e)
		return
	}
	token, e := Token()
	if e != nil {
		dbFail(w, e)
		return
	}
	csrf, e := Token()
	if e != nil {
		dbFail(w, e)
		return
	}
	if _, e = api.DB.ExecContext(r.Context(), "INSERT INTO sessions(token_hash,user_id,csrf,expires_at) VALUES($1,$2,$3,now()+interval '12 hours')", Hash(token), u.ID, csrf); e != nil {
		dbFail(w, e)
		return
	}
	// Header sessions work in Telegram Web's iframe without third-party cookies.
	writeJSON(w, 200, map[string]any{"token": token, "csrf": csrf, "user": u})
}

type botResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *botResponse) Header() http.Header { return r.header }
func (r *botResponse) WriteHeader(s int)   { r.status = s }
func (r *botResponse) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	return r.body.Write(p)
}

// BotRequest reuses the same validation, access checks and transactions as the website.
// There is no public HTTP endpoint that accepts an arbitrary Telegram ID.
func (a *App) BotRequest(ctx context.Context, tg TelegramUser, method, path string, body any) (json.RawMessage, error) {
	u, e := a.TelegramAccount(ctx, tg)
	if e != nil {
		return nil, e
	}
	raw, e := json.Marshal(body)
	if e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(context.WithValue(ctx, sessionKey{}, session{User: u}), method, "http://internal/api"+path, bytes.NewReader(raw))
	if e != nil {
		return nil, e
	}
	res := &botResponse{header: make(http.Header)}
	api := &API{App: a}
	api.route(res, req)
	if res.status >= 400 {
		var v struct {
			Error string `json:"error"`
		}
		json.Unmarshal(res.body.Bytes(), &v)
		return nil, errors.New(v.Error)
	}
	return res.body.Bytes(), nil
}
