package bot

import (
	"artem-study/internal/app"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMoneyInput(t *testing.T) {
	for s, want := range map[string]int64{"0": 0, "1,50": 150, "1000000": 100000000, "123.01": 12301} {
		v, e := parseMoney(s)
		if e != nil || v != want {
			t.Fatal(s, v, e)
		}
	}
	for _, s := range []string{"-1", "NaN", "1.001", "1000000.01", "1e3", "", "999999999999999999999"} {
		if _, e := parseMoney(s); e == nil {
			t.Fatal("accepted", s)
		}
	}
}
func TestTelegramWorkflow(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL missing")
	}
	ctx := context.Background()
	base, e := sql.Open("pgx", dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer base.Close()
	schema := fmt.Sprintf("telegram_test_%d", time.Now().UnixNano())
	if _, e = base.Exec("CREATE SCHEMA " + schema); e != nil {
		t.Fatal(e)
	}
	defer base.Exec("DROP SCHEMA " + schema + " CASCADE")
	parsed, e := url.Parse(dsn)
	if e != nil {
		t.Fatal(e)
	}
	q := parsed.Query()
	q.Set("search_path", schema+",public")
	parsed.RawQuery = q.Encode()
	db, e := sql.Open("pgx", parsed.String())
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, e = db.Exec("SET search_path TO " + schema + ",public"); e != nil {
		t.Fatal(e)
	}
	loc, _ := time.LoadLocation("Europe/Moscow")
	a := &app.App{DB: db, Config: app.Config{Origin: "https://study.example.org", BotToken: "test-token", AdminTelegramID: 100, AdminLogin: "admin", AdminPassword: "q", Location: loc}}
	if e = a.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	if e = a.Bootstrap(ctx); e != nil {
		t.Fatal(e)
	}
	outputs := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if s, ok := body["text"].(string); ok {
			outputs = append(outputs, s)
		}
		if markup, ok := body["reply_markup"].(map[string]any); ok {
			if rows, ok := markup["inline_keyboard"].([]any); ok {
				for _, row := range rows {
					for _, button := range row.([]any) {
						if v, ok := button.(map[string]any)["callback_data"].(string); ok && len(v) > 64 {
							t.Errorf("callback exceeds Telegram limit: %s", v)
						}
					}
				}
			}
		}
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	}))
	defer srv.Close()
	b := New(a)
	b.BaseURL = srv.URL + "/"
	b.Client = srv.Client()
	admin := app.TelegramUser{ID: 100, Username: "teacher"}
	message := func(tg app.TelegramUser, text string) {
		t.Helper()
		m := &Message{From: &tg, Text: text}
		m.Chat.ID = tg.ID
		m.Chat.Type = "private"
		if e := b.Handle(ctx, m); e != nil {
			t.Fatal(e)
		}
	}
	click := func(tg app.TelegramUser, data string) {
		t.Helper()
		m := &Message{}
		m.Chat.ID = tg.ID
		m.Chat.Type = "private"
		if e := b.HandleCallback(ctx, &Callback{ID: "callback", From: tg, Message: m, Data: data}); e != nil {
			t.Fatal(e)
		}
	}
	confirm := func(tg app.TelegramUser) string {
		t.Helper()
		d := b.dialogs[tg.ID]
		if d == nil || d.Step != -1 {
			t.Fatalf("not at confirmation: %#v", d)
		}
		data := "form:" + d.Nonce + ":-1:save"
		click(tg, data)
		return data
	}
	message(admin, "/start")
	click(admin, "new:students")
	for _, value := range []string{"Ученик Один", "@pupil_one", "", "заметка", "q"} {
		message(admin, value)
	}
	confirm(admin)
	var sid int64
	if e = db.QueryRow("SELECT student_id FROM users WHERE login='pupil_one'").Scan(&sid); e != nil {
		t.Fatal(e)
	}
	pupil := app.TelegramUser{ID: 200, Username: "Pupil_One"}
	message(pupil, "/start")
	account, e := a.TelegramAccount(ctx, pupil)
	if e != nil || account.StudentID != sid {
		t.Fatal(account, e)
	}
	testMiniAPI(t, a, pupil)
	// Username changes cannot transfer an established account to a different Telegram ID.
	if _, e = a.TelegramAccount(ctx, app.TelegramUser{ID: 201, Username: "pupil_one"}); e == nil {
		t.Fatal("stolen username got account")
	}
	pupil.Username = "renamed"
	if _, e = a.TelegramAccount(ctx, pupil); e != nil {
		t.Fatal(e)
	}
	if _, e = a.TelegramAccount(ctx, app.TelegramUser{ID: 999, Username: "admin"}); e == nil {
		t.Fatal("username escalated privileges")
	}
	click(pupil, "new:payments")
	if b.dialogs[pupil.ID] != nil {
		t.Fatal("student mutation form")
	}
	// Future lesson, duplicate callback and overlap rejection use the website rules.
	click(admin, "new:lessons")
	start := time.Now().In(loc).Add(48 * time.Hour).Truncate(time.Hour)
	end := start.Add(time.Hour)
	for _, value := range []string{num(sid), "Гитара", start.Format("02.01.2006 15:04"), end.Format("02.01.2006 15:04"), "1500", "", "1"} {
		message(admin, value)
	}
	callback := confirm(admin)
	click(admin, callback)
	var count int
	db.QueryRow("SELECT count(*) FROM lessons").Scan(&count)
	if count != 1 {
		t.Fatal("duplicate lesson", count)
	}
	var lid int64
	db.QueryRow("SELECT id FROM lessons").Scan(&lid)
	click(admin, "edit:lessons:"+num(lid))
	d := b.dialogs[admin.ID]
	d.Data["status"] = "completed"
	confirm(admin)
	db.QueryRow("SELECT count(*) FROM lessons WHERE status='completed'").Scan(&count)
	if count != 0 {
		t.Fatal("future marked completed")
	}
	message(admin, "Отмена")
	click(admin, "new:homework")
	for _, value := range []string{num(sid), "Гамма", "Играть медленно", end.Format("02.01.2006 15:04"), "https://example.org/material"} {
		message(admin, value)
	}
	confirm(admin)
	var hid int64
	db.QueryRow("SELECT id FROM homework").Scan(&hid)
	click(pupil, "show:homework:"+num(hid))
	click(pupil, "submit:"+num(hid))
	message(pupil, "Готово")
	message(pupil, "")
	confirm(pupil)
	var answer string
	db.QueryRow("SELECT answer FROM homework WHERE id=$1", hid).Scan(&answer)
	if answer != "Готово" {
		t.Fatal(answer)
	}
	click(admin, "new:payments")
	for _, value := range []string{num(sid), "1500,50", time.Now().In(loc).Format("02.01.2006 15:04"), "cash", "за урок"} {
		message(admin, value)
	}
	callback = confirm(admin)
	click(admin, callback)
	db.QueryRow("SELECT count(*) FROM payments").Scan(&count)
	if count != 1 {
		t.Fatal("duplicate payment", count)
	}
	var pid int64
	db.QueryRow("SELECT id FROM payments").Scan(&pid)
	click(admin, "edit:void:"+num(pid))
	message(admin, "ошибка")
	confirm(admin)
	// Mini App and bot share authorization. Students cannot call teacher endpoints.
	if _, e = a.BotRequest(ctx, pupil, "POST", "/payments", map[string]any{}); e == nil {
		t.Fatal("student wrote payment")
	}
	for _, text := range []string{"Расписание", "Домашние задания", "Адрес", "Приложение", "Ученики", "Доход и оплаты"} {
		message(admin, text)
	}
	for _, text := range []string{"Расписание", "Домашние задания", "Адрес", "Приложение"} {
		message(pupil, text)
	}
	click(admin, "choose:lessons:0")
	click(admin, "filterset:schedule:"+num(sid))
	click(admin, "period:month")
	click(admin, "edit:students:"+num(sid))
	d = b.dialogs[admin.ID]
	d.Data["active"] = false
	confirm(admin)
	if _, e = a.TelegramAccount(ctx, pupil); e == nil {
		t.Fatal("archived user still active")
	}
	db.Exec("UPDATE students SET active=true WHERE id=$1", sid)
	if _, e = a.TelegramAccount(ctx, app.TelegramUser{ID: 201, Username: "pupil_one"}); e == nil {
		t.Fatal("archive lost identity pin")
	}
	click(admin, "edit:unlink:"+num(sid))
	confirm(admin)
	if _, e = a.TelegramAccount(ctx, app.TelegramUser{ID: 201, Username: "pupil_one"}); e != nil {
		t.Fatal("explicit unlink did not release", e)
	}
	if !strings.Contains(strings.Join(outputs, "\n"), app.LessonAddress) {
		t.Fatal("missing address")
	}
}

func testMiniAPI(t *testing.T, a *app.App, tg app.TelegramUser) {
	t.Helper()
	rawUser, _ := json.Marshal(tg)
	q := url.Values{"user": {string(rawUser)}, "auth_date": {strconv.FormatInt(time.Now().Unix(), 10)}}
	keys := []string{}
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := []string{}
	for _, k := range keys {
		lines = append(lines, k+"="+q.Get(k))
	}
	key := hmac.New(sha256.New, []byte("WebAppData"))
	key.Write([]byte(a.Config.BotToken))
	mac := hmac.New(sha256.New, key.Sum(nil))
	mac.Write([]byte(strings.Join(lines, "\n")))
	q.Set("hash", hex.EncodeToString(mac.Sum(nil)))
	h := a.Handler(http.NotFoundHandler())
	body, _ := json.Marshal(map[string]string{"init_data": q.Encode()})
	req := func(method, path, token, csrf string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(string(body)))
		r.Header.Set("Origin", a.Config.Origin)
		r.Header.Set("X-Mini-App-Session", token)
		r.Header.Set("X-CSRF-Token", csrf)
		r.AddCookie(&http.Cookie{Name: "artem_session", Value: "old-cookie"})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := req("POST", "/api/telegram/login", "", "", body)
	if w.Code != 200 {
		t.Fatal("mini login", w.Code, w.Body.String())
	}
	var s struct {
		Token, CSRF string
		User        app.User
	}
	json.Unmarshal(w.Body.Bytes(), &s)
	if s.Token == "" || s.User.Role != "student" {
		t.Fatal("wrong mini identity")
	}
	w = req("GET", "/api/me", s.Token, "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pupil_one") {
		t.Fatal("header session", w.Code, w.Body.String())
	}
	if w = req("POST", "/api/students", s.Token, s.CSRF, []byte(`{}`)); w.Code != 403 {
		t.Fatal("student API privilege escalation", w.Code)
	}
	if w = req("POST", "/api/logout", s.Token, "", []byte(`{}`)); w.Code != 403 {
		t.Fatal("csrf missing accepted")
	}
	if w = req("POST", "/api/logout", s.Token, s.CSRF, []byte(`{}`)); w.Code != 200 {
		t.Fatal("mini logout", w.Code)
	}
	if w = req("GET", "/api/me", s.Token, "", nil); w.Code != 401 {
		t.Fatal("session not revoked")
	}
	q.Set("user", `{"id":100}`)
	body, _ = json.Marshal(map[string]string{"init_data": q.Encode()})
	if w = req("POST", "/api/telegram/login", "", "", body); w.Code != 401 {
		t.Fatal("forged admin accepted")
	}
}
