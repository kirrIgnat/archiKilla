package app

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type sessionKey struct{}
type session struct {
	User User
	CSRF string
}
type API struct {
	*App
	mu       sync.Mutex
	attempts map[string]attempt
}
type attempt struct {
	Count int
	Until time.Time
}

func (a *App) Handler(static http.Handler) http.Handler {
	api := &API{App: a, attempts: map[string]attempt{}}
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if a.DB.PingContext(ctx) != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	m.HandleFunc("POST /api/login", api.login)
	m.Handle("/api/", api.auth(http.HandlerFunc(api.route)))
	m.Handle("/", static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if a.Config.Secure {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if r.Header.Get("Origin") != a.Config.Origin {
				fail(w, 403, "Запрос с другого сайта отклонен")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		}
		m.ServeHTTP(w, r)
	})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, "Проверьте заполненные поля")
		return false
	}
	if d.Decode(&struct{}{}) != io.EOF {
		fail(w, 400, "Некорректный JSON")
		return false
	}
	return true
}
func dbFail(w http.ResponseWriter, e error) {
	if errors.Is(e, sql.ErrNoRows) {
		fail(w, 404, "Запись не найдена")
		return
	}
	if IsConflict(e) {
		fail(w, 409, "Запись конфликтует с существующими данными. Проверьте логин, ученика и пересечения занятий.")
		return
	}
	log.Printf("database operation failed (%T)", e)
	fail(w, 500, "Не удалось сохранить или загрузить данные")
}
func (api *API) allowLogin(ip string) bool {
	api.mu.Lock()
	defer api.mu.Unlock()
	now := time.Now()
	for k, v := range api.attempts {
		if now.After(v.Until) {
			delete(api.attempts, k)
		}
	}
	v := api.attempts[ip]
	if v.Count >= 15 {
		return false
	}
	if v.Count == 0 {
		v.Until = now.Add(10 * time.Minute)
	}
	v.Count++
	api.attempts[ip] = v
	return true
}
func (api *API) cookie(w http.ResponseWriter, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: "artem_session", Value: value, Path: "/", HttpOnly: true, Secure: api.Config.Secure, SameSite: http.SameSiteStrictMode, MaxAge: age})
}
func (api *API) login(w http.ResponseWriter, r *http.Request) {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !api.allowLogin(ip) {
		fail(w, 429, "Слишком много попыток. Повторите через 10 минут.")
		return
	}
	var input struct {
		Login    string `json:"login"`
		Password string `json:"password"`
	}
	if !decode(w, r, &input) {
		return
	}
	if len(input.Password) > 72 || len(input.Login) > 64 {
		fail(w, 401, "Неверный логин или пароль")
		return
	}
	var u User
	var h string
	e := api.DB.QueryRowContext(r.Context(), `SELECT u.id,u.login,u.role,COALESCE(u.student_id,0),u.password_hash FROM users u LEFT JOIN students s ON s.id=u.student_id WHERE u.login=$1 AND (u.role='admin' OR s.active)`, strings.ToLower(strings.TrimSpace(input.Login))).Scan(&u.ID, &u.Login, &u.Role, &u.StudentID, &h)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		dbFail(w, e)
		return
	}
	if errors.Is(e, sql.ErrNoRows) {
		h = "$2a$10$7EqJtq98hPqEX7fNZaFWoO5.d5F.RCF8EqR4XfW8I8LhLZ8Y8B2nK"
	}
	valid := bcrypt.CompareHashAndPassword([]byte(h), []byte(input.Password)) == nil
	if e != nil || !valid {
		fail(w, 401, "Неверный логин или пароль")
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
	tx, e := api.DB.BeginTx(r.Context(), nil)
	if e != nil {
		dbFail(w, e)
		return
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(r.Context(), "DELETE FROM sessions WHERE expires_at<now()"); e != nil {
		dbFail(w, e)
		return
	}
	if old, err := r.Cookie("artem_session"); err == nil {
		if _, e = tx.ExecContext(r.Context(), "DELETE FROM sessions WHERE token_hash=$1", Hash(old.Value)); e != nil {
			dbFail(w, e)
			return
		}
	}
	_, e = tx.ExecContext(r.Context(), "INSERT INTO sessions(token_hash,user_id,csrf,expires_at) VALUES($1,$2,$3,now()+interval '7 days')", Hash(token), u.ID, csrf)
	if e != nil {
		dbFail(w, e)
		return
	}
	if e = tx.Commit(); e != nil {
		dbFail(w, e)
		return
	}
	api.mu.Lock()
	delete(api.attempts, ip)
	api.mu.Unlock()
	api.cookie(w, token, 7*86400)
	writeJSON(w, 200, map[string]any{"user": u, "csrf": csrf})
}
func (api *API) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, e := r.Cookie("artem_session")
		if e != nil {
			fail(w, 401, "Войдите в кабинет")
			return
		}
		var s session
		e = api.DB.QueryRowContext(r.Context(), `SELECT u.id,u.login,u.role,COALESCE(u.student_id,0),t.csrf FROM sessions t JOIN users u ON u.id=t.user_id LEFT JOIN students p ON p.id=u.student_id WHERE t.token_hash=$1 AND t.expires_at>now() AND (u.role='admin' OR p.active)`, Hash(c.Value)).Scan(&s.User.ID, &s.User.Login, &s.User.Role, &s.User.StudentID, &s.CSRF)
		if errors.Is(e, sql.ErrNoRows) {
			fail(w, 401, "Войдите в кабинет")
			return
		}
		if e != nil {
			dbFail(w, e)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && subtle.ConstantTimeCompare([]byte(s.CSRF), []byte(r.Header.Get("X-CSRF-Token"))) != 1 {
			fail(w, 403, "Обновите страницу и повторите действие")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, s)))
	})
}
func (api *API) route(w http.ResponseWriter, r *http.Request) {
	s := r.Context().Value(sessionKey{}).(session)
	u := s.User
	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")
	id := int64(0)
	if len(parts) >= 3 && oneOf(parts[1], "students", "lessons", "homework", "payments") {
		var e error
		id, e = strconv.ParseInt(parts[2], 10, 64)
		if e != nil || id < 1 {
			fail(w, 404, "Не найдено")
			return
		}
	}
	if r.Method == "GET" {
		scope := int64(0)
		if u.Role == "student" {
			scope = u.StudentID
		}
		switch path {
		case "api/me":
			writeJSON(w, 200, map[string]any{"user": u, "csrf": s.CSRF, "timezone": api.Config.Location.String(), "bot_username": api.Config.BotUsername})
			return
		case "api/students":
			v, e := api.Students(r.Context(), scope)
			if e != nil {
				dbFail(w, e)
				return
			}
			if u.Role != "admin" {
				for i := range v {
					v[i].Notes = ""
				}
			}
			writeJSON(w, 200, v)
			return
		case "api/lessons":
			from, e1 := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
			to, e2 := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
			if e1 != nil || e2 != nil || !to.After(from) || to.Sub(from) > 370*24*time.Hour {
				fail(w, 400, "Выберите период до одного года")
				return
			}
			v, e := api.Lessons(r.Context(), scope, from, to)
			if e != nil {
				dbFail(w, e)
				return
			}
			if u.Role != "admin" {
				for i := range v {
					v[i].Notes = ""
				}
			}
			writeJSON(w, 200, v)
			return
		case "api/homework":
			v, e := api.HomeworkList(r.Context(), scope)
			if e != nil {
				dbFail(w, e)
				return
			}
			writeJSON(w, 200, v)
			return
		case "api/payments":
			v, e := api.Payments(r.Context(), scope)
			if e != nil {
				dbFail(w, e)
				return
			}
			writeJSON(w, 200, v)
			return
		}
	}
	if r.Method == "POST" && path == "api/logout" {
		c, _ := r.Cookie("artem_session")
		_, e := api.DB.ExecContext(r.Context(), "DELETE FROM sessions WHERE token_hash=$1", Hash(c.Value))
		if e != nil {
			dbFail(w, e)
			return
		}
		api.cookie(w, "", -1)
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	if r.Method == "POST" && path == "api/password" {
		api.password(w, r, u)
		return
	}
	if r.Method == "POST" && path == "api/telegram/invite" {
		api.invite(w, r, u)
		return
	}
	if r.Method == "POST" && path == "api/telegram/unlink" {
		api.unlink(w, r, u)
		return
	}
	if len(parts) == 4 && parts[1] == "homework" && parts[3] == "submit" && r.Method == "POST" && u.Role == "student" {
		api.submit(w, r, id, u)
		return
	}
	if u.Role != "admin" {
		fail(w, 403, "Доступ только для преподавателя")
		return
	}
	switch {
	case path == "api/students" && r.Method == "POST":
		api.saveStudent(w, r, 0)
	case len(parts) == 3 && parts[1] == "students" && r.Method == "PUT":
		api.saveStudent(w, r, id)
	case len(parts) == 4 && parts[1] == "students" && parts[3] == "password" && r.Method == "POST":
		api.resetPassword(w, r, id)
	case path == "api/lessons" && r.Method == "POST":
		api.saveLesson(w, r, 0)
	case len(parts) == 3 && parts[1] == "lessons" && r.Method == "PUT":
		api.saveLesson(w, r, id)
	case path == "api/homework" && r.Method == "POST":
		api.saveHomework(w, r, 0)
	case len(parts) == 3 && parts[1] == "homework" && r.Method == "PUT":
		api.saveHomework(w, r, id)
	case path == "api/payments" && r.Method == "POST":
		api.addPayment(w, r)
	case len(parts) == 4 && parts[1] == "payments" && parts[3] == "void" && r.Method == "POST":
		api.voidPayment(w, r, id)
	default:
		fail(w, 404, "Не найдено")
	}
}
func (api *API) password(w http.ResponseWriter, r *http.Request, u User) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decode(w, r, &in) {
		return
	}
	var old string
	e := api.DB.QueryRowContext(r.Context(), "SELECT password_hash FROM users WHERE id=$1", u.ID).Scan(&old)
	if e != nil {
		dbFail(w, e)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(old), []byte(in.Current)) != nil {
		fail(w, 400, "Текущий пароль неверен")
		return
	}
	h, e := PasswordHash(in.New)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	tx, e := api.DB.BeginTx(r.Context(), nil)
	if e != nil {
		dbFail(w, e)
		return
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(r.Context(), "UPDATE users SET password_hash=$1 WHERE id=$2", h, u.ID); e != nil {
		dbFail(w, e)
		return
	}
	if _, e = tx.ExecContext(r.Context(), "DELETE FROM sessions WHERE user_id=$1", u.ID); e != nil {
		dbFail(w, e)
		return
	}
	if e = tx.Commit(); e != nil {
		dbFail(w, e)
		return
	}
	api.cookie(w, "", -1)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (api *API) resetPassword(w http.ResponseWriter, r *http.Request, id int64) {
	var in struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	hash, e := PasswordHash(in.Password)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	tx, e := api.DB.BeginTx(r.Context(), nil)
	if e != nil {
		dbFail(w, e)
		return
	}
	defer tx.Rollback()
	var uid int64
	if e = tx.QueryRowContext(r.Context(), "UPDATE users SET password_hash=$1 WHERE student_id=$2 RETURNING id", hash, id).Scan(&uid); e != nil {
		dbFail(w, e)
		return
	}
	if _, e = tx.ExecContext(r.Context(), "DELETE FROM sessions WHERE user_id=$1", uid); e != nil {
		dbFail(w, e)
		return
	}
	if e = tx.Commit(); e != nil {
		dbFail(w, e)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
