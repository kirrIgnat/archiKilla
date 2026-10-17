package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"artem-study/internal/app"
	"artem-study/internal/bot"
	"artem-study/internal/web"
)

type client struct {
	t      *testing.T
	h      http.Handler
	cookie *http.Cookie
	csrf   string
}

func (c *client) req(method, path string, body any, status int) []byte {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if e := json.NewEncoder(&buf).Encode(body); e != nil {
			c.t.Fatal(e)
		}
	}
	r := httptest.NewRequest(method, "http://localhost:8083/api"+path, &buf)
	r.Header.Set("Origin", "http://localhost:8083")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", c.csrf)
	if c.cookie != nil {
		r.AddCookie(c.cookie)
	}
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, r)
	if w.Code != status {
		c.t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, status, w.Body.String())
	}
	for _, cookie := range w.Result().Cookies() {
		c.cookie = cookie
	}
	return w.Body.Bytes()
}
func (c *client) login(login, password string) {
	c.t.Helper()
	b := c.req("POST", "/login", map[string]string{"login": login, "password": password}, 200)
	var v struct {
		CSRF string `json:"csrf"`
	}
	json.Unmarshal(b, &v)
	c.csrf = v.CSRF
}
func idOf(t *testing.T, b []byte) int64 {
	t.Helper()
	var v struct {
		ID int64 `json:"id"`
	}
	if e := json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	if v.ID == 0 {
		t.Fatal("missing id")
	}
	return v.ID
}
func TestWorkflow(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	loc, _ := time.LoadLocation("Europe/Moscow")
	cfg := app.Config{DatabaseURL: dsn, Origin: "http://localhost:8083", Location: loc, AdminLogin: "artem", AdminPassword: "a-test-password-12345", AdminTelegramID: 9001}
	base, e := app.Open(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer base.DB.Close()
	schema := fmt.Sprintf("test_%d", time.Now().UnixNano())
	if _, e = base.DB.ExecContext(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	defer base.DB.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE")
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	cfg.DatabaseURL = u.String()
	a, e := app.Open(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer a.DB.Close()
	a.DB.SetMaxOpenConns(1)
	a.DB.SetMaxIdleConns(1)
	if e = a.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	if e = a.Migrate(ctx); e != nil {
		t.Fatal("repeat migration", e)
	}
	if e = a.Bootstrap(ctx); e != nil {
		t.Fatal(e)
	}
	h := a.Handler(web.Handler())
	teacher := &client{t: t, h: h}
	teacher.login("artem", cfg.AdminPassword)
	anon := &client{t: t, h: h}
	anon.req("GET", "/students", nil, 401)
	s1 := idOf(t, teacher.req("POST", "/students", map[string]any{"name": "Ученик Один", "phone": "123", "subject": "Гитара", "notes": "PRIVATE_NOTE", "active": true, "login": "student1", "password": "student-password-123"}, 200))
	s2 := idOf(t, teacher.req("POST", "/students", map[string]any{"name": "Ученик Два", "subject": "Вокал", "active": true, "login": "student2", "password": "student-password-456"}, 200))
	student := &client{t: t, h: h}
	student.login("student1", "student-password-123")
	b := student.req("GET", "/students", nil, 200)
	if strings.Contains(string(b), "Ученик Два") || strings.Contains(string(b), "PRIVATE_NOTE") {
		t.Fatal("student information leak")
	}
	student.req("POST", "/students", map[string]any{}, 403)
	saved := teacher.csrf
	teacher.csrf = "wrong"
	teacher.req("POST", "/students", map[string]any{}, 403)
	teacher.csrf = saved
	r := httptest.NewRequest("POST", "http://localhost:8083/api/logout", strings.NewReader("{}"))
	r.AddCookie(teacher.cookie)
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("X-CSRF-Token", teacher.csrf)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross origin accepted")
	}
	start := time.Now().Add(-48 * time.Hour).Truncate(time.Minute)
	lesson := map[string]any{"student_id": s1, "title": "Гитара", "starts_at": start, "ends_at": start.Add(time.Hour), "location": "Студия 1", "price_kop": 200000, "status": "completed", "notes": "PRIVATE_LESSON", "repeat": 1}
	lid := idOf(t, teacher.req("POST", "/lessons", lesson, 200))
	lesson["student_id"] = s2
	teacher.req("POST", "/lessons", lesson, 409)
	// Adjacent intervals are allowed; overlapping repeated series are atomic.
	lesson["starts_at"] = start.Add(time.Hour)
	lesson["ends_at"] = start.Add(2 * time.Hour)
	lesson["status"] = "planned"
	teacher.req("POST", "/lessons", lesson, 200)
	lesson["starts_at"] = start.Add(-7 * 24 * time.Hour)
	lesson["ends_at"] = start.Add(-7*24*time.Hour + time.Hour)
	lesson["repeat"] = 2
	teacher.req("POST", "/lessons", lesson, 409)
	var count int
	if e = a.DB.QueryRowContext(ctx, "SELECT count(*) FROM lessons").Scan(&count); e != nil || count != 2 {
		t.Fatalf("recurrence not atomic: %d %v", count, e)
	}
	lesson["repeat"] = 1
	lesson["starts_at"] = time.Now().Add(time.Hour)
	lesson["ends_at"] = time.Now().Add(2 * time.Hour)
	lesson["status"] = "completed"
	teacher.req("POST", "/lessons", lesson, 400)
	dateQuery := "/lessons?from=" + url.QueryEscape(start.Add(-time.Hour).Format(time.RFC3339)) + "&to=" + url.QueryEscape(start.Add(3*time.Hour).Format(time.RFC3339))
	b = student.req("GET", dateQuery, nil, 200)
	if strings.Contains(string(b), "Ученик Два") || strings.Contains(string(b), "PRIVATE_LESSON") {
		t.Fatal("lesson privacy leak")
	}
	payment := map[string]any{"student_id": s1, "amount_kop": 150000, "paid_at": time.Now().Add(-time.Hour).Truncate(time.Second), "method": "transfer", "note": "Перевод", "request_id": "test-payment-unique-001"}
	pid := idOf(t, teacher.req("POST", "/payments", payment, 200))
	if pid != idOf(t, teacher.req("POST", "/payments", payment, 200)) {
		t.Fatal("payment duplicated")
	}
	payment["amount_kop"] = 160000
	teacher.req("POST", "/payments", payment, 409)
	list, e := a.Students(ctx, s1)
	if e != nil {
		t.Fatal(e)
	}
	if list[0].Charges-list[0].Paid != 50000 {
		t.Fatal("wrong balance")
	}
	student.req("POST", "/payments", payment, 403)
	hw := map[string]any{"student_id": s1, "title": "Аккорды", "body": "Записать упражнение", "resource_url": "https://example.com/music", "due_at": time.Now().Add(24 * time.Hour), "status": "assigned", "feedback": ""}
	hid := idOf(t, teacher.req("POST", "/homework", hw, 200))
	hw["student_id"] = s2
	hid2 := idOf(t, teacher.req("POST", "/homework", hw, 200))
	student.req("POST", fmt.Sprintf("/homework/%d/submit", hid2), map[string]string{"answer": "wrong owner", "answer_url": ""}, 404)
	student.req("POST", fmt.Sprintf("/homework/%d/submit", hid), map[string]string{"answer": "Готово", "answer_url": "javascript:alert(1)"}, 400)
	student.req("POST", fmt.Sprintf("/homework/%d/submit", hid), map[string]string{"answer": "Готово", "answer_url": "https://example.com/recording"}, 200)
	hw["student_id"] = s1
	hw["status"] = "reviewed"
	hw["feedback"] = "Хорошая работа"
	teacher.req("PUT", fmt.Sprintf("/homework/%d", hid), hw, 200)
	student.req("POST", fmt.Sprintf("/homework/%d/submit", hid), map[string]string{"answer": "replace reviewed"}, 404)
	// A student-supplied target id is ignored when generating their own invite.
	b = student.req("POST", "/telegram/invite", map[string]int64{"student_id": s2}, 200)
	var inv struct {
		Command string `json:"command"`
	}
	json.Unmarshal(b, &inv)
	var sent []string
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Text string `json:"text"`
		}
		json.NewDecoder(r.Body).Decode(&p)
		sent = append(sent, p.Text)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":{}}`)
	}))
	defer tg.Close()
	botClient := bot.New(a)
	botClient.BaseURL = tg.URL + "/"
	botClient.Client = tg.Client()
	msg := func(id int64, text string) *bot.Message {
		m := &bot.Message{}
		raw := fmt.Sprintf(`{"from":{"id":%d},"chat":{"id":%d,"type":"private"},"text":%q}`, id, id, text)
		if e := json.Unmarshal([]byte(raw), m); e != nil {
			t.Fatal(e)
		}
		return m
	}
	if e = botClient.Handle(ctx, msg(101, inv.Command)); e != nil {
		t.Fatal(e)
	}
	var linked int64
	if e = a.DB.QueryRowContext(ctx, "SELECT student_id FROM telegram_links WHERE telegram_id=101").Scan(&linked); e != nil || linked != s1 {
		t.Fatal("wrong telegram link", linked, e)
	}
	if e = botClient.Handle(ctx, msg(102, inv.Command)); e != nil {
		t.Fatal(e)
	}
	if e = a.DB.QueryRowContext(ctx, "SELECT count(*) FROM telegram_links").Scan(&count); e != nil || count != 1 {
		t.Fatal("invite reused")
	}
	if e = botClient.Handle(ctx, msg(101, "Расписание")); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(sent[len(sent)-1], "Ученик Два") {
		t.Fatal("telegram data leak")
	}
	student.req("POST", "/telegram/unlink", map[string]int64{"student_id": s2}, 200)
	if e = a.DB.QueryRowContext(ctx, "SELECT count(*) FROM telegram_links").Scan(&count); e != nil || count != 0 {
		t.Fatal("unlink failed")
	}
	teacher.req("POST", fmt.Sprintf("/payments/%d/void", pid), map[string]string{"reason": "Ошибочная запись"}, 200)
	list, e = a.Students(ctx, s1)
	if e != nil || list[0].Paid != 0 {
		t.Fatal("void balance failed", e)
	}
	// Cancelling a previously completed lesson reverses its charge, keeping history.
	lesson = map[string]any{"student_id": s1, "title": "Гитара", "starts_at": start, "ends_at": start.Add(time.Hour), "location": "Студия 1", "price_kop": 200000, "status": "cancelled", "notes": "", "repeat": 1}
	teacher.req("PUT", fmt.Sprintf("/lessons/%d", lid), lesson, 200)
	list, e = a.Students(ctx, s1)
	if e != nil || list[0].Charges != 0 {
		t.Fatal("cancelled lesson still billed")
	}
	teacher.req("PUT", fmt.Sprintf("/students/%d", s1), map[string]any{"name": "Ученик Один", "subject": "Гитара", "active": false, "login": "student1"}, 200)
	student.req("GET", "/me", nil, 401)
	anon.req("POST", "/login", map[string]string{"login": "student1", "password": "student-password-123"}, 401)
	// Assets must be served as usable files, not authentication redirects.
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("asset %s: %d", path, w.Code)
		}
	}
}
