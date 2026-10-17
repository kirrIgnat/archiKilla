// Package bot provides the Telegram interface for ARCHIKILLA-STUDY.
// It works both as a dedicated long-polling process and through the portfolio bridge.
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
	"strconv"
	"strings"
	"sync"
	"time"

	"artem-study/internal/app"
)

const lessonAddress = "Лесная 16, 10 подъезд, 12 этаж, квартира 666, код от домофона ключ123321ключ"

type Bot struct {
	App     *app.App
	Client  *http.Client
	BaseURL string

	flowMu sync.Mutex
	flows  map[int64]*flow
}

type flow struct {
	Kind       string
	Step       string
	StudentID  int64
	LessonID   int64
	HomeworkID int64
	PaymentID  int64
	Data       map[string]string
}

type studentIdentity struct {
	ID    int64
	Name  string
	Login string
}

type Message struct {
	From *struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Text string `json:"text"`
}

type Update struct {
	ID      int64    `json:"update_id"`
	Message *Message `json:"message"`
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
	return &Bot{
		App:     a,
		Client:  &http.Client{Timeout: 40 * time.Second, Transport: tr},
		BaseURL: "https://api.telegram.org/bot" + a.Config.BotToken + "/",
		flows:   map[int64]*flow{},
	}
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
	if res.StatusCode != http.StatusOK || !envelope.OK {
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
		e = b.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 25, "allowed_updates": []string{"message"}}, &updates)
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
				if e != nil {
					var ae *APIError
					if !(errors.As(e, &ae) && (ae.Code == 403 || ae.Code == 400)) {
						log.Printf("bot handler: %v", e)
						if !pause(ctx, 2*time.Second) {
							return ctx.Err()
						}
						break
					}
				}
			}
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

func keyboard(rows ...[]string) [][]map[string]string {
	out := make([][]map[string]string, 0, len(rows))
	for _, row := range rows {
		buttons := make([]map[string]string, 0, len(row))
		for _, label := range row {
			buttons = append(buttons, map[string]string{"text": label})
		}
		out = append(out, buttons)
	}
	return out
}

func (b *Bot) sendKeys(ctx context.Context, id int64, text string, keys [][]map[string]string) error {
	for _, part := range splitText(text) {
		payload := map[string]any{
			"chat_id": id,
			"text":    part,
			"reply_markup": map[string]any{
				"keyboard":          keys,
				"resize_keyboard":   true,
				"is_persistent":     true,
				"one_time_keyboard": false,
			},
			"link_preview_options": map[string]bool{"is_disabled": true},
		}
		if e := b.call(ctx, "sendMessage", payload, nil); e != nil {
			return e
		}
	}
	return nil
}

func (b *Bot) adminMainKeyboard() [][]map[string]string {
	rows := keyboard(
		[]string{"Расписание", "Ученики"},
		[]string{"Домашние задания", "Доход"},
	)
	if b.App.Config.BridgeSecret != "" {
		rows = append(rows, keyboard([]string{"Портфолио"})...)
	}
	return rows
}

func studentMainKeyboard() [][]map[string]string {
	return keyboard(
		[]string{"Расписание", "Домашние задания"},
		[]string{"Адрес"},
	)
}

func (b *Bot) showAdminMain(ctx context.Context, chatID int64, text string) error {
	b.clearFlow(chatID)
	if text == "" {
		text = "Кабинет преподавателя. Выберите раздел."
	}
	return b.sendKeys(ctx, chatID, text, b.adminMainKeyboard())
}

func (b *Bot) showStudentMain(ctx context.Context, chatID int64, s studentIdentity, text string) error {
	b.clearFlow(chatID)
	if text == "" {
		text = fmt.Sprintf("%s\nЛичный кабинет ученика. Выберите раздел.", s.Name)
	}
	return b.sendKeys(ctx, chatID, text, studentMainKeyboard())
}

func (b *Bot) setFlow(chatID int64, f *flow) {
	b.flowMu.Lock()
	defer b.flowMu.Unlock()
	if b.flows == nil {
		b.flows = map[int64]*flow{}
	}
	b.flows[chatID] = f
}

func (b *Bot) getFlow(chatID int64) *flow {
	b.flowMu.Lock()
	defer b.flowMu.Unlock()
	if b.flows == nil {
		return nil
	}
	return b.flows[chatID]
}

func (b *Bot) clearFlow(chatID int64) {
	b.flowMu.Lock()
	defer b.flowMu.Unlock()
	delete(b.flows, chatID)
}

func command(text string) string {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return ""
	}
	return strings.Split(fields[0], "@")[0]
}

func (b *Bot) telegramUsername(ctx context.Context, telegramID int64) (string, error) {
	var chat struct {
		Username string `json:"username"`
	}
	if e := b.call(ctx, "getChat", map[string]any{"chat_id": telegramID}, &chat); e != nil {
		return "", e
	}
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(chat.Username), "@")), nil
}

func (b *Bot) studentForTelegram(ctx context.Context, telegramID int64) (studentIdentity, error) {
	var s studentIdentity
	e := b.App.DB.QueryRowContext(ctx, `SELECT s.id,s.name,u.login
		FROM telegram_links t
		JOIN students s ON s.id=t.student_id
		JOIN users u ON u.student_id=s.id
		WHERE t.telegram_id=$1 AND s.active`, telegramID).Scan(&s.ID, &s.Name, &s.Login)
	if e == nil {
		return s, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return s, e
	}

	username, e := b.telegramUsername(ctx, telegramID)
	if e != nil {
		return s, e
	}
	if username == "" {
		return s, errors.New("У вашего Telegram-аккаунта нет username. Установите ник в Telegram и попросите преподавателя указать этот же ник как логин ученика.")
	}
	e = b.App.DB.QueryRowContext(ctx, `SELECT s.id,s.name,u.login
		FROM users u JOIN students s ON s.id=u.student_id
		WHERE u.role='student' AND s.active AND lower(ltrim(u.login,'@'))=$1`, username).Scan(&s.ID, &s.Name, &s.Login)
	if errors.Is(e, sql.ErrNoRows) {
		return s, fmt.Errorf("Профиль не найден. Ваш ник в Telegram: @%s. Логин ученика на сайте должен совпадать с ним.", username)
	}
	if e != nil {
		return s, e
	}
	res, e := b.App.DB.ExecContext(ctx, `INSERT INTO telegram_links(student_id,telegram_id) VALUES($1,$2)
		ON CONFLICT DO NOTHING`, s.ID, telegramID)
	if e != nil {
		return s, e
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return s, errors.New("Этот профиль уже привязан к другому Telegram. Отключите старую привязку в кабинете преподавателя.")
	}
	return s, nil
}

func (b *Bot) Handle(ctx context.Context, m *Message) error {
	if m.From == nil || m.Chat.Type != "private" || m.Chat.ID != m.From.ID {
		return nil
	}
	text := strings.TrimSpace(m.Text)
	cmd := command(text)
	admin := m.From.ID == b.App.Config.AdminTelegramID

	if admin {
		if cmd == "/start" || cmd == "/study" || text == "Главное меню" {
			return b.showAdminMain(ctx, m.Chat.ID, "Кабинет преподавателя. Здесь доступны расписание, ученики, домашние задания и доход.")
		}
		if f := b.getFlow(m.Chat.ID); f != nil {
			return b.handleAdminFlow(ctx, m.Chat.ID, text, f)
		}
		return b.handleAdminMain(ctx, m.Chat.ID, text, cmd)
	}

	s, e := b.studentForTelegram(ctx, m.From.ID)
	if e != nil {
		return b.sendKeys(ctx, m.Chat.ID, e.Error(), studentMainKeyboard())
	}
	if cmd == "/start" || text == "Главное меню" {
		return b.showStudentMain(ctx, m.Chat.ID, s, fmt.Sprintf("%s\nПрофиль подключен. Логин: %s", s.Name, s.Login))
	}
	if f := b.getFlow(m.Chat.ID); f != nil {
		return b.handleStudentFlow(ctx, m.Chat.ID, text, s, f)
	}
	return b.handleStudentMain(ctx, m.Chat.ID, text, cmd, s)
}

func (b *Bot) handleStudentMain(ctx context.Context, chatID int64, text, cmd string, s studentIdentity) error {
	switch {
	case text == "Расписание" || cmd == "/schedule" || cmd == "/today":
		b.setFlow(chatID, &flow{Kind: "student_schedule"})
		if cmd == "/today" {
			if e := b.sendSchedule(ctx, chatID, s.ID, false, "day"); e != nil {
				return e
			}
		}
		return b.sendKeys(ctx, chatID, "Расписание. Выберите период.", keyboard(
			[]string{"Сегодня", "Неделя", "Месяц"},
			[]string{"Главное меню"},
		))
	case text == "Домашние задания" || cmd == "/homework":
		if e := b.sendHomework(ctx, chatID, s.ID, false); e != nil {
			return e
		}
		b.setFlow(chatID, &flow{Kind: "student_homework"})
		return b.sendKeys(ctx, chatID, "Действия с домашними заданиями.", keyboard(
			[]string{"Отправить ответ"},
			[]string{"Главное меню"},
		))
	case text == "Адрес" || cmd == "/address":
		return b.showStudentMain(ctx, chatID, s, "Адрес проведения занятий:\n"+lessonAddress)
	default:
		return b.showStudentMain(ctx, chatID, s, "Выберите расписание, домашние задания или адрес.")
	}
}

func (b *Bot) handleStudentFlow(ctx context.Context, chatID int64, text string, s studentIdentity, f *flow) error {
	if text == "Главное меню" || text == "Отмена" {
		return b.showStudentMain(ctx, chatID, s, "Главное меню.")
	}
	switch f.Kind {
	case "student_schedule":
		period := map[string]string{"Сегодня": "day", "Неделя": "week", "Месяц": "month"}[text]
		if period == "" {
			return b.sendKeys(ctx, chatID, "Выберите период кнопкой.", keyboard([]string{"Сегодня", "Неделя", "Месяц"}, []string{"Главное меню"}))
		}
		if e := b.sendSchedule(ctx, chatID, s.ID, false, period); e != nil {
			return e
		}
		return b.sendKeys(ctx, chatID, "Выберите другой период или вернитесь в меню.", keyboard([]string{"Сегодня", "Неделя", "Месяц"}, []string{"Главное меню"}))
	case "student_homework":
		if text != "Отправить ответ" {
			return b.sendKeys(ctx, chatID, "Выберите действие кнопкой.", keyboard([]string{"Отправить ответ"}, []string{"Главное меню"}))
		}
		hw, e := b.App.HomeworkList(ctx, s.ID)
		if e != nil {
			return e
		}
		var out strings.Builder
		out.WriteString("Выберите задание. Отправьте его номер, например #12.\n")
		n := 0
		for _, h := range hw {
			if h.Status == "reviewed" {
				continue
			}
			n++
			fmt.Fprintf(&out, "\n#%d · %s · до %s\n", h.ID, h.Title, h.DueAt.In(b.App.Config.Location).Format("02.01 15:04"))
		}
		if n == 0 {
			return b.showStudentMain(ctx, chatID, s, "Нет заданий, на которые можно отправить ответ.")
		}
		b.setFlow(chatID, &flow{Kind: "student_submit", Step: "select"})
		return b.sendKeys(ctx, chatID, out.String(), keyboard([]string{"Отмена"}))
	case "student_submit":
		if f.Step == "select" {
			id, ok := parseNumberID(text)
			if !ok {
				return b.sendKeys(ctx, chatID, "Отправьте номер задания в формате #12.", keyboard([]string{"Отмена"}))
			}
			var exists bool
			e := b.App.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM homework WHERE id=$1 AND student_id=$2 AND status IN ('assigned','submitted'))", id, s.ID).Scan(&exists)
			if e != nil {
				return e
			}
			if !exists {
				return b.sendKeys(ctx, chatID, "Такого открытого задания у вас нет.", keyboard([]string{"Отмена"}))
			}
			b.setFlow(chatID, &flow{Kind: "student_submit", Step: "answer", HomeworkID: id})
			return b.sendKeys(ctx, chatID, "Отправьте ответ одним сообщением. Если ответом является ссылка, отправьте только http:// или https:// ссылку.", keyboard([]string{"Отмена"}))
		}
		answer := strings.TrimSpace(text)
		if answer == "" || len(answer) > 12000 {
			return b.sendKeys(ctx, chatID, "Ответ пустой или слишком длинный.", keyboard([]string{"Отмена"}))
		}
		answerText, answerURL := answer, ""
		if strings.HasPrefix(answer, "https://") || strings.HasPrefix(answer, "http://") {
			if !app.ValidURL(answer) {
				return b.sendKeys(ctx, chatID, "Некорректная ссылка.", keyboard([]string{"Отмена"}))
			}
			answerText, answerURL = "", answer
		}
		res, e := b.App.DB.ExecContext(ctx, "UPDATE homework SET answer=$1,answer_url=$2,status='submitted',feedback='' WHERE id=$3 AND student_id=$4 AND status IN ('assigned','submitted')", answerText, answerURL, f.HomeworkID, s.ID)
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return b.showStudentMain(ctx, chatID, s, "Задание уже закрыто или не найдено.")
		}
		return b.showStudentMain(ctx, chatID, s, "Ответ отправлен преподавателю.")
	}
	return b.showStudentMain(ctx, chatID, s, "Главное меню.")
}

func (b *Bot) handleAdminMain(ctx context.Context, chatID int64, text, cmd string) error {
	switch {
	case text == "Расписание" || cmd == "/schedule" || cmd == "/today":
		b.setFlow(chatID, &flow{Kind: "admin_schedule"})
		if cmd == "/today" {
			if e := b.sendSchedule(ctx, chatID, 0, true, "day"); e != nil {
				return e
			}
		}
		return b.sendKeys(ctx, chatID, "Расписание преподавателя.", adminScheduleKeyboard())
	case text == "Ученики" || cmd == "/students":
		b.setFlow(chatID, &flow{Kind: "admin_students"})
		return b.sendKeys(ctx, chatID, "Ученики.", keyboard(
			[]string{"Список учеников", "Добавить ученика"},
			[]string{"Изменить ученика"},
			[]string{"Главное меню"},
		))
	case text == "Домашние задания" || cmd == "/homework":
		b.setFlow(chatID, &flow{Kind: "admin_homework"})
		return b.sendKeys(ctx, chatID, "Домашние задания.", keyboard(
			[]string{"Все ДЗ", "Выставить ДЗ"},
			[]string{"Проверить ДЗ"},
			[]string{"Главное меню"},
		))
	case text == "Доход" || cmd == "/income" || cmd == "/balance":
		b.setFlow(chatID, &flow{Kind: "admin_income"})
		return b.sendKeys(ctx, chatID, "Доход и оплаты.", adminIncomeKeyboard())
	default:
		return b.showAdminMain(ctx, chatID, "Выберите раздел кнопкой.")
	}
}

func adminScheduleKeyboard() [][]map[string]string {
	return keyboard(
		[]string{"Сегодня", "Неделя", "Месяц"},
		[]string{"Добавить занятие", "Статус занятия"},
		[]string{"Главное меню"},
	)
}

func adminIncomeKeyboard() [][]map[string]string {
	return keyboard(
		[]string{"Сегодня", "Неделя", "Месяц"},
		[]string{"Добавить оплату", "Отменить оплату"},
		[]string{"Баланс учеников"},
		[]string{"Главное меню"},
	)
}

func (b *Bot) handleAdminFlow(ctx context.Context, chatID int64, text string, f *flow) error {
	if text == "Главное меню" {
		return b.showAdminMain(ctx, chatID, "Главное меню.")
	}
	if text == "Отмена" {
		return b.showAdminMain(ctx, chatID, "Действие отменено.")
	}
	switch f.Kind {
	case "admin_schedule":
		if period := map[string]string{"Сегодня": "day", "Неделя": "week", "Месяц": "month"}[text]; period != "" {
			if e := b.sendSchedule(ctx, chatID, 0, true, period); e != nil {
				return e
			}
			return b.sendKeys(ctx, chatID, "Расписание.", adminScheduleKeyboard())
		}
		switch text {
		case "Добавить занятие":
			b.setFlow(chatID, &flow{Kind: "lesson_add", Step: "student", Data: map[string]string{}})
			return b.sendStudentPicker(ctx, chatID, "Выберите ученика для занятия.")
		case "Статус занятия":
			return b.startLessonStatus(ctx, chatID)
		default:
			return b.sendKeys(ctx, chatID, "Выберите действие кнопкой.", adminScheduleKeyboard())
		}
	case "admin_students":
		switch text {
		case "Список учеников":
			if e := b.sendStudents(ctx, chatID); e != nil {
				return e
			}
			return b.sendKeys(ctx, chatID, "Ученики.", keyboard([]string{"Список учеников", "Добавить ученика"}, []string{"Изменить ученика"}, []string{"Главное меню"}))
		case "Добавить ученика":
			b.setFlow(chatID, &flow{Kind: "student_add", Step: "name", Data: map[string]string{}})
			return b.sendKeys(ctx, chatID, "Введите имя и фамилию ученика.", keyboard([]string{"Отмена"}))
		case "Изменить ученика":
			if e := b.sendStudents(ctx, chatID); e != nil {
				return e
			}
			b.setFlow(chatID, &flow{Kind: "student_edit", Step: "select", Data: map[string]string{}})
			return b.sendKeys(ctx, chatID, "Отправьте номер ученика, например #3.", keyboard([]string{"Отмена"}))
		default:
			return b.sendKeys(ctx, chatID, "Выберите действие кнопкой.", keyboard([]string{"Список учеников", "Добавить ученика"}, []string{"Изменить ученика"}, []string{"Главное меню"}))
		}
	case "admin_homework":
		switch text {
		case "Все ДЗ":
			if e := b.sendHomework(ctx, chatID, 0, true); e != nil {
				return e
			}
			return b.sendKeys(ctx, chatID, "Домашние задания.", keyboard([]string{"Все ДЗ", "Выставить ДЗ"}, []string{"Проверить ДЗ"}, []string{"Главное меню"}))
		case "Выставить ДЗ":
			b.setFlow(chatID, &flow{Kind: "homework_add", Step: "student", Data: map[string]string{}})
			return b.sendStudentPicker(ctx, chatID, "Выберите ученика.")
		case "Проверить ДЗ":
			return b.startHomeworkReview(ctx, chatID)
		default:
			return b.sendKeys(ctx, chatID, "Выберите действие кнопкой.", keyboard([]string{"Все ДЗ", "Выставить ДЗ"}, []string{"Проверить ДЗ"}, []string{"Главное меню"}))
		}
	case "admin_income":
		if period := map[string]string{"Сегодня": "day", "Неделя": "week", "Месяц": "month"}[text]; period != "" {
			if e := b.sendIncome(ctx, chatID, period); e != nil {
				return e
			}
			return b.sendKeys(ctx, chatID, "Доход и оплаты.", adminIncomeKeyboard())
		}
		switch text {
		case "Баланс учеников":
			if e := b.sendBalances(ctx, chatID); e != nil {
				return e
			}
			return b.sendKeys(ctx, chatID, "Доход и оплаты.", adminIncomeKeyboard())
		case "Добавить оплату":
			b.setFlow(chatID, &flow{Kind: "payment_add", Step: "student", Data: map[string]string{}})
			return b.sendStudentPicker(ctx, chatID, "Выберите ученика, от которого получена оплата.")
		case "Отменить оплату":
			return b.startPaymentVoid(ctx, chatID)
		default:
			return b.sendKeys(ctx, chatID, "Выберите действие кнопкой.", adminIncomeKeyboard())
		}
	case "student_add":
		return b.handleStudentAdd(ctx, chatID, text, f)
	case "student_edit":
		return b.handleStudentEdit(ctx, chatID, text, f)
	case "lesson_add":
		return b.handleLessonAdd(ctx, chatID, text, f)
	case "lesson_status":
		return b.handleLessonStatus(ctx, chatID, text, f)
	case "homework_add":
		return b.handleHomeworkAdd(ctx, chatID, text, f)
	case "homework_review":
		return b.handleHomeworkReview(ctx, chatID, text, f)
	case "payment_add":
		return b.handlePaymentAdd(ctx, chatID, text, f)
	case "payment_void":
		return b.handlePaymentVoid(ctx, chatID, text, f)
	default:
		return b.showAdminMain(ctx, chatID, "Главное меню.")
	}
}

func (b *Bot) sendStudents(ctx context.Context, chatID int64) error {
	students, e := b.App.Students(ctx, 0)
	if e != nil {
		return e
	}
	var out strings.Builder
	out.WriteString("Ученики\n")
	for _, s := range students {
		state := "активен"
		if !s.Active {
			state = "архив"
		}
		tg := "не подключен"
		if s.Telegram {
			tg = "Telegram подключен"
		}
		fmt.Fprintf(&out, "\n#%d · %s\nЛогин: %s\nТелефон: %s\n%s · %s\n", s.ID, s.Name, s.Login, emptyDash(s.Phone), state, tg)
	}
	if len(students) == 0 {
		out.WriteString("\nУчеников пока нет.")
	}
	return b.sendKeys(ctx, chatID, out.String(), keyboard([]string{"Главное меню"}))
}

func (b *Bot) sendStudentPicker(ctx context.Context, chatID int64, title string) error {
	students, e := b.App.Students(ctx, 0)
	if e != nil {
		return e
	}
	rows := [][]string{}
	row := []string{}
	for _, s := range students {
		if !s.Active {
			continue
		}
		row = append(row, fmt.Sprintf("#%d %s", s.ID, s.Name))
		if len(row) == 2 {
			rows = append(rows, row)
			row = []string{}
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []string{"Отмена"})
	return b.sendKeys(ctx, chatID, title, keyboard(rows...))
}

func (b *Bot) handleStudentAdd(ctx context.Context, chatID int64, text string, f *flow) error {
	if f.Data == nil {
		f.Data = map[string]string{}
	}
	switch f.Step {
	case "name":
		name := strings.TrimSpace(text)
		if name == "" || len(name) > 200 {
			return b.sendKeys(ctx, chatID, "Введите имя до 200 символов.", keyboard([]string{"Отмена"}))
		}
		f.Data["name"] = name
		f.Step = "login"
		b.setFlow(chatID, f)
		return b.sendKeys(ctx, chatID, "Введите Telegram username ученика без @. Этот же логин будет использоваться на сайте.", keyboard([]string{"Отмена"}))
	case "login":
		login := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(text), "@"))
		if !app.ValidLogin(login) {
			return b.sendKeys(ctx, chatID, "Некорректный логин. Используйте Telegram username без @.", keyboard([]string{"Отмена"}))
		}
		f.Data["login"] = login
		f.Step = "phone"
		b.setFlow(chatID, f)
		return b.sendKeys(ctx, chatID, "Введите телефон или отправьте '-' если он не нужен.", keyboard([]string{"-"}, []string{"Отмена"}))
	case "phone":
		phone := strings.TrimSpace(text)
		if phone == "-" {
			phone = ""
		}
		if len(phone) > 100 {
			return b.sendKeys(ctx, chatID, "Телефон слишком длинный.", keyboard([]string{"-"}, []string{"Отмена"}))
		}
		f.Data["phone"] = phone
		f.Step = "password"
		b.setFlow(chatID, f)
		return b.sendKeys(ctx, chatID, "Введите первый пароль ученика. От 12 до 72 символов.", keyboard([]string{"Отмена"}))
	case "password":
		hash, e := app.PasswordHash(text)
		if e != nil {
			return b.sendKeys(ctx, chatID, e.Error(), keyboard([]string{"Отмена"}))
		}
		tx, e := b.App.DB.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		var sid int64
		e = tx.QueryRowContext(ctx, "INSERT INTO students(name,phone,subject,notes,active) VALUES($1,$2,'Музыка','',true) RETURNING id", f.Data["name"], f.Data["phone"]).Scan(&sid)
		if e == nil {
			_, e = tx.ExecContext(ctx, "INSERT INTO users(login,password_hash,role,student_id) VALUES($1,$2,'student',$3)", f.Data["login"], hash, sid)
		}
		if e != nil {
			if app.IsConflict(e) {
				return b.sendKeys(ctx, chatID, "Такой логин уже существует. Действие отменено.", b.adminMainKeyboard())
			}
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
		return b.showAdminMain(ctx, chatID, fmt.Sprintf("Ученик %s создан. Логин: %s. При первом сообщении боту Telegram-профиль привяжется автоматически по username.", f.Data["name"], f.Data["login"]))
	}
	return b.showAdminMain(ctx, chatID, "Главное меню.")
}

func (b *Bot) handleStudentEdit(ctx context.Context, chatID int64, text string, f *flow) error {
	switch f.Step {
	case "select":
		id, ok := parseNumberID(text)
		if !ok {
			return b.sendKeys(ctx, chatID, "Отправьте номер ученика, например #3.", keyboard([]string{"Отмена"}))
		}
		var active bool
		if e := b.App.DB.QueryRowContext(ctx, "SELECT active FROM students WHERE id=$1", id).Scan(&active); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return b.sendKeys(ctx, chatID, "Ученик не найден.", keyboard([]string{"Отмена"}))
			}
			return e
		}
		f.StudentID = id
		f.Step = "action"
		b.setFlow(chatID, f)
		archiveLabel := "Архивировать"
		if !active {
			archiveLabel = "Активировать"
		}
		return b.sendKeys(ctx, chatID, "Что изменить?", keyboard(
			[]string{"Имя", "Телефон", "Логин"},
			[]string{"Пароль", archiveLabel},
			[]string{"Отмена"},
		))
	case "action":
		switch text {
		case "Имя", "Телефон", "Логин", "Пароль":
			f.Data = map[string]string{"field": text}
			f.Step = "value"
			b.setFlow(chatID, f)
			prompt := map[string]string{"Имя": "Введите новое имя.", "Телефон": "Введите новый телефон или '-'.", "Логин": "Введите новый Telegram username без @.", "Пароль": "Введите новый пароль от 12 до 72 символов."}[text]
			return b.sendKeys(ctx, chatID, prompt, keyboard([]string{"Отмена"}))
		case "Архивировать":
			tx, e := b.App.DB.BeginTx(ctx, nil)
			if e != nil {
				return e
			}
			defer tx.Rollback()
			if _, e = tx.ExecContext(ctx, "UPDATE students SET active=false WHERE id=$1", f.StudentID); e == nil {
				_, e = tx.ExecContext(ctx, "DELETE FROM telegram_links WHERE student_id=$1", f.StudentID)
			}
			if e == nil {
				_, e = tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id=(SELECT id FROM users WHERE student_id=$1)", f.StudentID)
			}
			if e != nil {
				return e
			}
			if e = tx.Commit(); e != nil {
				return e
			}
			return b.showAdminMain(ctx, chatID, "Ученик перенесен в архив.")
		case "Активировать":
			if _, e := b.App.DB.ExecContext(ctx, "UPDATE students SET active=true WHERE id=$1", f.StudentID); e != nil {
				return e
			}
			return b.showAdminMain(ctx, chatID, "Ученик активирован.")
		default:
			return b.sendKeys(ctx, chatID, "Выберите действие кнопкой.", keyboard([]string{"Имя", "Телефон", "Логин"}, []string{"Пароль", "Архивировать"}, []string{"Отмена"}))
		}
	case "value":
		field := f.Data["field"]
		switch field {
		case "Имя":
			value := strings.TrimSpace(text)
			if value == "" || len(value) > 200 {
				return b.sendKeys(ctx, chatID, "Некорректное имя.", keyboard([]string{"Отмена"}))
			}
			_, e := b.App.DB.ExecContext(ctx, "UPDATE students SET name=$1 WHERE id=$2", value, f.StudentID)
			if e != nil {
				return e
			}
		case "Телефон":
			value := strings.TrimSpace(text)
			if value == "-" {
				value = ""
			}
			if len(value) > 100 {
				return b.sendKeys(ctx, chatID, "Телефон слишком длинный.", keyboard([]string{"Отмена"}))
			}
			_, e := b.App.DB.ExecContext(ctx, "UPDATE students SET phone=$1 WHERE id=$2", value, f.StudentID)
			if e != nil {
				return e
			}
		case "Логин":
			value := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(text), "@"))
			if !app.ValidLogin(value) {
				return b.sendKeys(ctx, chatID, "Некорректный Telegram username.", keyboard([]string{"Отмена"}))
			}
			tx, e := b.App.DB.BeginTx(ctx, nil)
			if e != nil {
				return e
			}
			defer tx.Rollback()
			_, e = tx.ExecContext(ctx, "UPDATE users SET login=$1 WHERE student_id=$2", value, f.StudentID)
			if e == nil {
				_, e = tx.ExecContext(ctx, "DELETE FROM telegram_links WHERE student_id=$1", f.StudentID)
			}
			if e != nil {
				if app.IsConflict(e) {
					return b.sendKeys(ctx, chatID, "Такой логин уже используется.", keyboard([]string{"Отмена"}))
				}
				return e
			}
			if e = tx.Commit(); e != nil {
				return e
			}
		case "Пароль":
			hash, e := app.PasswordHash(text)
			if e != nil {
				return b.sendKeys(ctx, chatID, e.Error(), keyboard([]string{"Отмена"}))
			}
			if _, e = b.App.DB.ExecContext(ctx, "UPDATE users SET password_hash=$1 WHERE student_id=$2", hash, f.StudentID); e != nil {
				return e
			}
		}
		return b.showAdminMain(ctx, chatID, "Данные ученика обновлены.")
	}
	return b.showAdminMain(ctx, chatID, "Главное меню.")
}

func (b *Bot) handleLessonAdd(ctx context.Context, chatID int64, text string, f *flow) error {
	if f.Data == nil {
		f.Data = map[string]string{}
	}
	switch f.Step {
	case "student":
		id, ok := parseNumberID(text)
		if !ok {
			return b.sendStudentPicker(ctx, chatID, "Выберите ученика кнопкой.")
		}
		f.StudentID = id
		f.Step = "start"
		b.setFlow(chatID, f)
		return b.sendKeys(ctx, chatID, "Введите дату и время начала: ДД.ММ.ГГГГ ЧЧ:ММ", keyboard([]string{"Отмена"}))
	case "start":
		start, e := parseLocalDateTime(text, b.App.Config.Location)
		if e != nil {
			return b.sendKeys(ctx, chatID, "Не понял дату. Пример: 25.10.2026 18:30", keyboard([]string{"Отмена"}))
		}
		f.Data["start"] = start.Format(time.RFC3339)
		f.Step = "duration"
		b.setFlow(chatID, f)
		return b.sendKeys(ctx, chatID, "Длительность занятия в минутах.", keyboard([]string{"60", "90", "120"}, []string{"Отмена"}))
	case "duration":
		minutes, e := strconv.Atoi(strings.TrimSpace(text))
		if e != nil || minutes < 5 || minutes > 720 {
			return b.sendKeys(ctx, chatID, "Введите число от 5 до 720.", keyboard([]string{"60", "90", "120"}, []string{"Отмена"}))
		}
		f.Data["duration"] = strconv.Itoa(minutes)
		f.Step = "title"
		b.setFlow(chatID, f)
		return b.sendKeys(ctx, chatID, "Название занятия.", keyboard([]string{"Занятие"}, []string{"Отмена"}))
	case "title":
		title := strings.TrimSpace(text)
		if title == "" || len(title) > 200 {
			return b.sendKeys(ctx, chatID, "Введите название до 200 символов.", keyboard([]string{"Занятие"}, []string{"Отмена"}))
		}
		f.Data["title"] = title
		f.Step = "price"
		b.setFlow(chatID, f)
		return b.sendKeys(ctx, chatID, "Стоимость занятия в рублях. Можно указать 0.", keyboard([]string{"0"}, []string{"Отмена"}))
	case "price":
		price, e := parseRubles(text, true)
		if e != nil || price > 100000000 {
			return b.sendKeys(ctx, chatID, "Некорректная сумма.", keyboard([]string{"0"}, []string{"Отмена"}))
		}
		f.Data["price"] = strconv.FormatInt(price, 10)
		f.Step = "repeat"
		b.setFlow(chatID, f)
		return b.sendKeys(ctx, chatID, "Сколько недель повторять занятие? От 1 до 12.", keyboard([]string{"1", "4", "8", "12"}, []string{"Отмена"}))
	case "repeat":
		repeat, e := strconv.Atoi(strings.TrimSpace(text))
		if e != nil || repeat < 1 || repeat > 12 {
			return b.sendKeys(ctx, chatID, "Введите число от 1 до 12.", keyboard([]string{"1", "4", "8", "12"}, []string{"Отмена"}))
		}
		start, _ := time.Parse(time.RFC3339, f.Data["start"])
		duration, _ := strconv.Atoi(f.Data["duration"])
		price, _ := strconv.ParseInt(f.Data["price"], 10, 64)
		tx, e := b.App.DB.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		var active bool
		if e = tx.QueryRowContext(ctx, "SELECT active FROM students WHERE id=$1 FOR UPDATE", f.StudentID).Scan(&active); e != nil {
			return e
		}
		if !active {
			return b.showAdminMain(ctx, chatID, "Ученик находится в архиве.")
		}
		for i := 0; i < repeat; i++ {
			s := start.In(b.App.Config.Location).AddDate(0, 0, 7*i)
			eTime := s.Add(time.Duration(duration) * time.Minute)
			if _, e = tx.ExecContext(ctx, `INSERT INTO lessons(student_id,title,starts_at,ends_at,location,price_kop,status,notes)
				VALUES($1,$2,$3,$4,$5,$6,'planned','')`, f.StudentID, f.Data["title"], s, eTime, lessonAddress, price); e != nil {
				if app.IsConflict(e) {
					return b.showAdminMain(ctx, chatID, "Не удалось добавить занятие: это время пересекается с другим занятием.")
				}
				return e
			}
		}
		if e = tx.Commit(); e != nil {
			return e
		}
		return b.showAdminMain(ctx, chatID, fmt.Sprintf("Занятие добавлено. Повторов: %d. Адрес: %s", repeat, lessonAddress))
	}
	return b.showAdminMain(ctx, chatID, "Главное меню.")
}

func (b *Bot) startLessonStatus(ctx context.Context, chatID int64) error {
	now := time.Now().In(b.App.Config.Location)
	lessons, e := b.App.Lessons(ctx, 0, now.AddDate(0, -1, 0), now.AddDate(0, 3, 0))
	if e != nil {
		return e
	}
	var out strings.Builder
	out.WriteString("Занятия. Отправьте номер занятия, например #18.\n")
	start := 0
	if len(lessons) > 30 {
		start = len(lessons) - 30
	}
	for _, l := range lessons[start:] {
		fmt.Fprintf(&out, "\n#%d · %s · %s\n%s · %s\n", l.ID, l.StartsAt.In(b.App.Config.Location).Format("02.01 15:04"), l.StudentName, l.Title, lessonStatus(l.Status))
	}
	if len(lessons) == 0 {
		return b.showAdminMain(ctx, chatID, "Занятий для изменения статуса нет.")
	}
	b.setFlow(chatID, &flow{Kind: "lesson_status", Step: "select"})
	return b.sendKeys(ctx, chatID, out.String(), keyboard([]string{"Отмена"}))
}

func (b *Bot) handleLessonStatus(ctx context.Context, chatID int64, text string, f *flow) error {
	if f.Step == "select" {
		id, ok := parseNumberID(text)
		if !ok {
			return b.sendKeys(ctx, chatID, "Введите номер занятия, например #18.", keyboard([]string{"Отмена"}))
		}
		var exists bool
		if e := b.App.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM lessons WHERE id=$1)", id).Scan(&exists); e != nil {
			return e
		}
		if !exists {
			return b.sendKeys(ctx, chatID, "Занятие не найдено.", keyboard([]string{"Отмена"}))
		}
		f.LessonID = id
		f.Step = "status"
		b.setFlow(chatID, f)
		return b.sendKeys(ctx, chatID, "Выберите новый статус.", keyboard([]string{"Запланировано", "Проведено"}, []string{"Отменено", "Пропуск"}, []string{"Отмена"}))
	}
	status := map[string]string{"Запланировано": "planned", "Проведено": "completed", "Отменено": "cancelled", "Пропуск": "missed"}[text]
	if status == "" {
		return b.sendKeys(ctx, chatID, "Выберите статус кнопкой.", keyboard([]string{"Запланировано", "Проведено"}, []string{"Отменено", "Пропуск"}, []string{"Отмена"}))
	}
	if status == "completed" {
		var end time.Time
		if e := b.App.DB.QueryRowContext(ctx, "SELECT ends_at FROM lessons WHERE id=$1", f.LessonID).Scan(&end); e != nil {
			return e
		}
		if end.After(time.Now()) {
			return b.sendKeys(ctx, chatID, "Будущее занятие еще нельзя отметить проведенным.", keyboard([]string{"Запланировано", "Отменено", "Пропуск"}, []string{"Отмена"}))
		}
	}
	if _, e := b.App.DB.ExecContext(ctx, "UPDATE lessons SET status=$1 WHERE id=$2", status, f.LessonID); e != nil {
		return e
	}
	return b.showAdminMain(ctx, chatID, "Статус занятия обновлен.")
}

func (b *Bot) handleHomeworkAdd(ctx context.Context, chatID int64, text string, f *flow) error {
	if f.Data == nil {
		f.Data = map[string]string{}
	}
	switch f.Step {
	case "student":
		id, ok := parseNumberID(text)
		if !ok {
			return b.sendStudentPicker(ctx, chatID, "Выберите ученика кнопкой.")
		}
		f.StudentID = id
		f.Step = "title"
		b.setFlow(chatID, f)
		return b.sendKeys(ctx, chatID, "Название домашнего задания.", keyboard([]string{"Отмена"}))
	case "title":
		value := strings.TrimSpace(text)
		if value == "" || len(value) > 200 {
			return b.sendKeys(ctx, chatID, "Введите название до 200 символов.", keyboard([]string{"Отмена"}))
		}
		f.Data["title"] = value
		f.Step = "body"
		b.setFlow(chatID, f)
		return b.sendKeys(ctx, chatID, "Текст задания.", keyboard([]string{"Отмена"}))
	case "body":
		value := strings.TrimSpace(text)
		if value == "" || len(value) > 12000 {
			return b.sendKeys(ctx, chatID, "Текст задания пустой или слишком длинный.", keyboard([]string{"Отмена"}))
		}
		f.Data["body"] = value
		f.Step = "due"
		b.setFlow(chatID, f)
		return b.sendKeys(ctx, chatID, "Срок: ДД.ММ.ГГГГ ЧЧ:ММ", keyboard([]string{"Отмена"}))
	case "due":
		due, e := parseLocalDateTime(text, b.App.Config.Location)
		if e != nil {
			return b.sendKeys(ctx, chatID, "Не понял дату. Пример: 01.11.2026 20:00", keyboard([]string{"Отмена"}))
		}
		f.Data["due"] = due.Format(time.RFC3339)
		f.Step = "url"
		b.setFlow(chatID, f)
		return b.sendKeys(ctx, chatID, "Ссылка на материал или '-' если ссылки нет.", keyboard([]string{"-"}, []string{"Отмена"}))
	case "url":
		url := strings.TrimSpace(text)
		if url == "-" {
			url = ""
		}
		if !app.ValidURL(url) {
			return b.sendKeys(ctx, chatID, "Ссылка должна начинаться с http:// или https://", keyboard([]string{"-"}, []string{"Отмена"}))
		}
		due, _ := time.Parse(time.RFC3339, f.Data["due"])
		var id int64
		e := b.App.DB.QueryRowContext(ctx, `INSERT INTO homework(student_id,title,body,resource_url,due_at,status)
			VALUES($1,$2,$3,$4,$5,'assigned') RETURNING id`, f.StudentID, f.Data["title"], f.Data["body"], url, due).Scan(&id)
		if e != nil {
			return e
		}
		return b.showAdminMain(ctx, chatID, fmt.Sprintf("Домашнее задание #%d выдано.", id))
	}
	return b.showAdminMain(ctx, chatID, "Главное меню.")
}

func (b *Bot) startHomeworkReview(ctx context.Context, chatID int64) error {
	hw, e := b.App.HomeworkList(ctx, 0)
	if e != nil {
		return e
	}
	var out strings.Builder
	out.WriteString("Работы на проверке. Отправьте номер задания.\n")
	n := 0
	for _, h := range hw {
		if h.Status != "submitted" {
			continue
		}
		n++
		fmt.Fprintf(&out, "\n#%d · %s · %s\n%s\n", h.ID, h.StudentName, h.Title, emptyDash(h.Answer))
		if h.AnswerURL != "" {
			out.WriteString(h.AnswerURL + "\n")
		}
	}
	if n == 0 {
		return b.showAdminMain(ctx, chatID, "Работ на проверке сейчас нет.")
	}
	b.setFlow(chatID, &flow{Kind: "homework_review", Step: "select"})
	return b.sendKeys(ctx, chatID, out.String(), keyboard([]string{"Отмена"}))
}

func (b *Bot) handleHomeworkReview(ctx context.Context, chatID int64, text string, f *flow) error {
	if f.Step == "select" {
		id, ok := parseNumberID(text)
		if !ok {
			return b.sendKeys(ctx, chatID, "Введите номер задания, например #7.", keyboard([]string{"Отмена"}))
		}
		var exists bool
		if e := b.App.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM homework WHERE id=$1 AND status='submitted')", id).Scan(&exists); e != nil {
			return e
		}
		if !exists {
			return b.sendKeys(ctx, chatID, "Работа не найдена или уже проверена.", keyboard([]string{"Отмена"}))
		}
		f.HomeworkID = id
		f.Step = "feedback"
		b.setFlow(chatID, f)
		return b.sendKeys(ctx, chatID, "Введите комментарий ученику или '-' без комментария.", keyboard([]string{"-"}, []string{"Отмена"}))
	}
	feedback := strings.TrimSpace(text)
	if feedback == "-" {
		feedback = ""
	}
	if len(feedback) > 6000 {
		return b.sendKeys(ctx, chatID, "Комментарий слишком длинный.", keyboard([]string{"-"}, []string{"Отмена"}))
	}
	res, e := b.App.DB.ExecContext(ctx, "UPDATE homework SET status='reviewed',feedback=$1 WHERE id=$2 AND status='submitted'", feedback, f.HomeworkID)
	if e != nil {
		return e
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return b.showAdminMain(ctx, chatID, "Работа уже была проверена.")
	}
	return b.showAdminMain(ctx, chatID, "Домашнее задание проверено.")
}

func (b *Bot) handlePaymentAdd(ctx context.Context, chatID int64, text string, f *flow) error {
	if f.Data == nil {
		f.Data = map[string]string{}
	}
	switch f.Step {
	case "student":
		id, ok := parseNumberID(text)
		if !ok {
			return b.sendStudentPicker(ctx, chatID, "Выберите ученика кнопкой.")
		}
		f.StudentID = id
		f.Step = "amount"
		b.setFlow(chatID, f)
		return b.sendKeys(ctx, chatID, "Сумма оплаты в рублях.", keyboard([]string{"Отмена"}))
	case "amount":
		amount, e := parseRubles(text, false)
		if e != nil || amount < 1 || amount > 100000000 {
			return b.sendKeys(ctx, chatID, "Некорректная сумма.", keyboard([]string{"Отмена"}))
		}
		f.Data["amount"] = strconv.FormatInt(amount, 10)
		f.Step = "method"
		b.setFlow(chatID, f)
		return b.sendKeys(ctx, chatID, "Способ оплаты.", keyboard([]string{"Наличные", "Перевод", "Другое"}, []string{"Отмена"}))
	case "method":
		method := map[string]string{"Наличные": "cash", "Перевод": "transfer", "Другое": "other"}[text]
		if method == "" {
			return b.sendKeys(ctx, chatID, "Выберите способ кнопкой.", keyboard([]string{"Наличные", "Перевод", "Другое"}, []string{"Отмена"}))
		}
		f.Data["method"] = method
		f.Step = "note"
		b.setFlow(chatID, f)
		return b.sendKeys(ctx, chatID, "Комментарий к оплате или '-'.", keyboard([]string{"-"}, []string{"Отмена"}))
	case "note":
		note := strings.TrimSpace(text)
		if note == "-" {
			note = ""
		}
		if len(note) > 2000 {
			return b.sendKeys(ctx, chatID, "Комментарий слишком длинный.", keyboard([]string{"-"}, []string{"Отмена"}))
		}
		amount, _ := strconv.ParseInt(f.Data["amount"], 10, 64)
		token, e := app.Token()
		if e != nil {
			return e
		}
		var id int64
		e = b.App.DB.QueryRowContext(ctx, `INSERT INTO payments(student_id,amount_kop,paid_at,method,note,request_id)
			VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, f.StudentID, amount, time.Now(), f.Data["method"], note, "tg-"+token).Scan(&id)
		if e != nil {
			return e
		}
		return b.showAdminMain(ctx, chatID, fmt.Sprintf("Оплата #%d добавлена: %s.", id, app.Money(amount)))
	}
	return b.showAdminMain(ctx, chatID, "Главное меню.")
}

func (b *Bot) startPaymentVoid(ctx context.Context, chatID int64) error {
	rows, e := b.App.DB.QueryContext(ctx, `SELECT p.id,s.name,p.amount_kop,p.paid_at,p.method
		FROM payments p JOIN students s ON s.id=p.student_id
		WHERE p.void_reason='' ORDER BY p.paid_at DESC,p.id DESC LIMIT 20`)
	if e != nil {
		return e
	}
	defer rows.Close()
	var out strings.Builder
	out.WriteString("Последние оплаты. Отправьте номер оплаты для отмены.\n")
	n := 0
	for rows.Next() {
		var id, amount int64
		var name, method string
		var paidAt time.Time
		if e = rows.Scan(&id, &name, &amount, &paidAt, &method); e != nil {
			return e
		}
		n++
		fmt.Fprintf(&out, "\n#%d · %s · %s · %s\n", id, name, app.Money(amount), paidAt.In(b.App.Config.Location).Format("02.01.2006"))
	}
	if e = rows.Err(); e != nil {
		return e
	}
	if n == 0 {
		return b.showAdminMain(ctx, chatID, "Активных оплат нет.")
	}
	b.setFlow(chatID, &flow{Kind: "payment_void", Step: "select"})
	return b.sendKeys(ctx, chatID, out.String(), keyboard([]string{"Отмена"}))
}

func (b *Bot) handlePaymentVoid(ctx context.Context, chatID int64, text string, f *flow) error {
	if f.Step == "select" {
		id, ok := parseNumberID(text)
		if !ok {
			return b.sendKeys(ctx, chatID, "Введите номер оплаты, например #5.", keyboard([]string{"Отмена"}))
		}
		var exists bool
		if e := b.App.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payments WHERE id=$1 AND void_reason='')", id).Scan(&exists); e != nil {
			return e
		}
		if !exists {
			return b.sendKeys(ctx, chatID, "Оплата не найдена или уже отменена.", keyboard([]string{"Отмена"}))
		}
		f.PaymentID = id
		f.Step = "reason"
		b.setFlow(chatID, f)
		return b.sendKeys(ctx, chatID, "Укажите причину отмены оплаты.", keyboard([]string{"Отмена"}))
	}
	reason := strings.TrimSpace(text)
	if reason == "" || len(reason) > 1000 {
		return b.sendKeys(ctx, chatID, "Укажите причину до 1000 символов.", keyboard([]string{"Отмена"}))
	}
	res, e := b.App.DB.ExecContext(ctx, "UPDATE payments SET void_reason=$1 WHERE id=$2 AND void_reason=''", reason, f.PaymentID)
	if e != nil {
		return e
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return b.showAdminMain(ctx, chatID, "Оплата уже была отменена.")
	}
	return b.showAdminMain(ctx, chatID, "Оплата отменена.")
}

func (b *Bot) sendSchedule(ctx context.Context, chatID, studentID int64, admin bool, period string) error {
	from, to, title := periodBounds(time.Now().In(b.App.Config.Location), period)
	lessons, e := b.App.Lessons(ctx, studentID, from, to)
	if e != nil {
		return e
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Расписание · %s\n", title)
	if len(lessons) == 0 {
		out.WriteString("\nЗанятий нет.")
	}
	for _, l := range lessons {
		start := l.StartsAt.In(b.App.Config.Location)
		end := l.EndsAt.In(b.App.Config.Location)
		if admin {
			fmt.Fprintf(&out, "\n#%d · %s %s-%s\n%s · %s\n%s · %s\n", l.ID, start.Format("02.01"), start.Format("15:04"), end.Format("15:04"), l.StudentName, l.Title, lessonStatus(l.Status), app.Money(l.Price))
		} else {
			fmt.Fprintf(&out, "\n%s %s-%s\n%s\n%s\n", start.Format("02.01"), start.Format("15:04"), end.Format("15:04"), l.Title, lessonStatus(l.Status))
		}
	}
	if !admin {
		out.WriteString("\n\nАдрес: " + lessonAddress)
	}
	return b.sendKeys(ctx, chatID, out.String(), func() [][]map[string]string {
		if admin {
			return adminScheduleKeyboard()
		}
		return keyboard([]string{"Сегодня", "Неделя", "Месяц"}, []string{"Главное меню"})
	}())
}

func (b *Bot) sendHomework(ctx context.Context, chatID, studentID int64, admin bool) error {
	hw, e := b.App.HomeworkList(ctx, studentID)
	if e != nil {
		return e
	}
	var out strings.Builder
	out.WriteString("Домашние задания\n")
	if len(hw) == 0 {
		out.WriteString("\nЗаданий пока нет.")
	}
	for _, h := range hw {
		if admin {
			fmt.Fprintf(&out, "\n#%d · %s · %s\n", h.ID, h.StudentName, h.Title)
		} else {
			fmt.Fprintf(&out, "\n#%d · %s\n", h.ID, h.Title)
		}
		fmt.Fprintf(&out, "До %s · %s\n%s\n", h.DueAt.In(b.App.Config.Location).Format("02.01.2006 15:04"), homeworkStatus(h.Status), h.Body)
		if h.ResourceURL != "" {
			out.WriteString("Материал: " + h.ResourceURL + "\n")
		}
		if h.Answer != "" {
			out.WriteString("Ответ: " + h.Answer + "\n")
		}
		if h.AnswerURL != "" {
			out.WriteString("Ответ: " + h.AnswerURL + "\n")
		}
		if h.Feedback != "" {
			out.WriteString("Комментарий: " + h.Feedback + "\n")
		}
	}
	keys := studentMainKeyboard()
	if admin {
		keys = keyboard([]string{"Все ДЗ", "Выставить ДЗ"}, []string{"Проверить ДЗ"}, []string{"Главное меню"})
	}
	return b.sendKeys(ctx, chatID, out.String(), keys)
}

func (b *Bot) sendIncome(ctx context.Context, chatID int64, period string) error {
	from, to, title := periodBounds(time.Now().In(b.App.Config.Location), period)
	rows, e := b.App.DB.QueryContext(ctx, `SELECT s.name,COALESCE(sum(p.amount_kop),0)
		FROM payments p JOIN students s ON s.id=p.student_id
		WHERE p.void_reason='' AND p.paid_at >= $1 AND p.paid_at < $2
		GROUP BY s.id,s.name ORDER BY s.name`, from, to)
	if e != nil {
		return e
	}
	defer rows.Close()
	var out strings.Builder
	fmt.Fprintf(&out, "Доход · %s\n", title)
	var total int64
	for rows.Next() {
		var name string
		var amount int64
		if e = rows.Scan(&name, &amount); e != nil {
			return e
		}
		total += amount
		fmt.Fprintf(&out, "\n%s: %s", name, app.Money(amount))
	}
	if e = rows.Err(); e != nil {
		return e
	}
	var charges int64
	if e = b.App.DB.QueryRowContext(ctx, "SELECT COALESCE(sum(price_kop),0) FROM lessons WHERE status='completed' AND starts_at >= $1 AND starts_at < $2", from, to).Scan(&charges); e != nil {
		return e
	}
	fmt.Fprintf(&out, "\n\nПолучено: %s\nПроведено занятий на: %s", app.Money(total), app.Money(charges))
	return b.sendKeys(ctx, chatID, out.String(), adminIncomeKeyboard())
}

func (b *Bot) sendBalances(ctx context.Context, chatID int64) error {
	students, e := b.App.Students(ctx, 0)
	if e != nil {
		return e
	}
	var out strings.Builder
	out.WriteString("Баланс учеников\n")
	for _, s := range students {
		if !s.Active {
			continue
		}
		balance := s.Paid - s.Charges
		label := "Баланс"
		if balance < 0 {
			label = "Долг"
			balance = -balance
		}
		fmt.Fprintf(&out, "\n%s\n%s: %s\n", s.Name, label, app.Money(balance))
	}
	return b.sendKeys(ctx, chatID, out.String(), adminIncomeKeyboard())
}

func periodBounds(now time.Time, period string) (time.Time, time.Time, string) {
	loc := now.Location()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	switch period {
	case "day":
		return day, day.AddDate(0, 0, 1), day.Format("02.01.2006")
	case "month":
		from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
		return from, from.AddDate(0, 1, 0), from.Format("01.2006")
	default:
		shift := (int(now.Weekday()) + 6) % 7
		from := day.AddDate(0, 0, -shift)
		to := from.AddDate(0, 0, 7)
		return from, to, fmt.Sprintf("%s-%s", from.Format("02.01"), to.Add(-time.Second).Format("02.01.2006"))
	}
}

func parseLocalDateTime(value string, loc *time.Location) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"02.01.2006 15:04", "02.01.06 15:04"} {
		if t, e := time.ParseInLocation(layout, value, loc); e == nil {
			return t, nil
		}
	}
	if t, e := time.ParseInLocation("02.01 15:04", value, loc); e == nil {
		now := time.Now().In(loc)
		return time.Date(now.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc), nil
	}
	return time.Time{}, errors.New("invalid datetime")
}

func parseRubles(value string, allowZero bool) (int64, error) {
	value = strings.ReplaceAll(strings.TrimSpace(value), " ", "")
	value = strings.ReplaceAll(value, ",", ".")
	parts := strings.Split(value, ".")
	if len(parts) > 2 || len(parts) == 0 || parts[0] == "" {
		return 0, errors.New("invalid amount")
	}
	rubles, e := strconv.ParseInt(parts[0], 10, 64)
	if e != nil || rubles < 0 {
		return 0, errors.New("invalid amount")
	}
	kopeks := int64(0)
	if len(parts) == 2 {
		if len(parts[1]) == 1 {
			parts[1] += "0"
		}
		if len(parts[1]) != 2 {
			return 0, errors.New("invalid amount")
		}
		kopeks, e = strconv.ParseInt(parts[1], 10, 64)
		if e != nil || kopeks < 0 || kopeks > 99 {
			return 0, errors.New("invalid amount")
		}
	}
	amount := rubles*100 + kopeks
	if !allowZero && amount == 0 {
		return 0, errors.New("invalid amount")
	}
	return amount, nil
}

func parseNumberID(text string) (int64, bool) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return 0, false
	}
	raw := strings.TrimPrefix(fields[0], "#")
	id, e := strconv.ParseInt(raw, 10, 64)
	return id, e == nil && id > 0
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func lessonStatus(s string) string {
	if v := map[string]string{"planned": "запланировано", "completed": "проведено", "cancelled": "отменено", "missed": "пропуск"}[s]; v != "" {
		return v
	}
	return s
}

func homeworkStatus(s string) string {
	if v := map[string]string{"assigned": "выдано", "submitted": "на проверке", "reviewed": "проверено"}[s]; v != "" {
		return v
	}
	return s
}
