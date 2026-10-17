package bot

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"artem-study/internal/app"
)

type Callback struct {
	ID      string           `json:"id"`
	From    app.TelegramUser `json:"from"`
	Message *Message         `json:"message"`
	Data    string           `json:"data"`
}
type button map[string]any
type keyboard [][]button

func key(label, data string) button   { return button{"text": label, "callback_data": data} }
func row(label, data string) []button { return []button{key(label, data)} }
func num(n int64) string              { return strconv.FormatInt(n, 10) }
func number(s string) int64           { n, _ := strconv.ParseInt(s, 10, 64); return n }
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

type view struct {
	From, To time.Time
	Month    bool
	Student  int64
	Until    time.Time
}
type dialog struct {
	Kind, Nonce string
	ID          int64
	Data        map[string]any
	Fields      []field
	Step        int
	Editing     bool
	Until       time.Time
	Scope       int64
}
type field struct {
	Key, Label, Type string
	Options          []choice
	Optional         bool
}
type choice struct{ Label, Value string }

func opt(label, value string) choice { return choice{label, value} }
func f(k, l, t string) field         { return field{Key: k, Label: l, Type: t} }
func optional(k, l, t string) field  { return field{Key: k, Label: l, Type: t, Optional: true} }
func enum(k, l string, values ...choice) field {
	return field{Key: k, Label: l, Type: "enum", Options: values}
}

func (b *Bot) state(id int64) *view {
	if b.views == nil {
		b.views = map[int64]*view{}
	}
	if b.dialogs == nil {
		b.dialogs = map[int64]*dialog{}
	}
	now := time.Now()
	for k, v := range b.views {
		if now.After(v.Until) {
			delete(b.views, k)
		}
	}
	for k, v := range b.dialogs {
		if now.After(v.Until) {
			delete(b.dialogs, k)
		}
	}
	v := b.views[id]
	if v == nil {
		n := now.In(b.App.Config.Location)
		start := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location())
		v = &view{From: start, To: start.AddDate(0, 0, 7)}
		b.views[id] = v
	}
	v.Until = now.Add(time.Hour)
	return v
}
func (b *Bot) send(ctx context.Context, id int64, text string, admin bool) error {
	keys := [][]map[string]string{{{"text": "Расписание"}, {"text": "Домашние задания"}}, {{"text": "Адрес"}}}
	if b.miniAppAvailable() {
		keys[1] = append(keys[1], map[string]string{"text": "Приложение"})
	}
	if admin {
		keys = append(keys, []map[string]string{{"text": "Ученики"}, {"text": "Доход и оплаты"}})
	}
	keys = append(keys, []map[string]string{{"text": "Меню"}, {"text": "Отмена"}})
	return b.message(ctx, id, text, map[string]any{"keyboard": keys, "resize_keyboard": true})
}
func (b *Bot) message(ctx context.Context, id int64, text string, markup any) error {
	parts := splitText(text)
	for i, p := range parts {
		payload := map[string]any{"chat_id": id, "text": p, "link_preview_options": map[string]bool{"is_disabled": true}}
		if i == len(parts)-1 && markup != nil {
			payload["reply_markup"] = markup
		}
		if e := b.call(ctx, "sendMessage", payload, nil); e != nil {
			return e
		}
	}
	return nil
}
func (b *Bot) screen(ctx context.Context, id int64, text string, k keyboard) error {
	return b.message(ctx, id, text, map[string]any{"inline_keyboard": k})
}
func (b *Bot) account(ctx context.Context, tg app.TelegramUser) (app.User, error) {
	return b.App.TelegramAccount(ctx, tg)
}
func (b *Bot) Handle(ctx context.Context, m *Message) error {
	if m.From == nil || m.Chat.Type != "private" || m.Chat.ID != m.From.ID {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state(m.From.ID)
	text := strings.TrimSpace(m.Text)
	args := strings.Fields(text)
	if len(args) == 2 && args[0] == "/start" && m.From.ID != b.App.Config.AdminTelegramID {
		if e := b.link(ctx, args[1], m.From.ID); e != nil {
			return b.send(ctx, m.Chat.ID, "Не удалось привязать аккаунт. Попросите преподавателя проверить логин или выдать новую ссылку.", false)
		}
	}
	u, e := b.account(ctx, *m.From)
	if e != nil {
		if errors.Is(e, app.ErrTelegramAccount) {
			return b.send(ctx, m.Chat.ID, e.Error(), false)
		}
		return e
	}
	admin := u.Role == "admin"
	commands := map[string]string{"Расписание": "schedule", "/schedule": "schedule", "/today": "today", "Домашние задания": "hw", "/homework": "hw", "Адрес": "address", "Приложение": "app", "Кабинет": "app", "Ученики": "students", "Доход и оплаты": "payments", "Оплата": "payments", "Меню": "menu", "Отмена": "menu", "/cancel": "menu", "/start": "menu", "/menu": "menu"}
	if len(args) > 0 && args[0] == "/start" {
		text = "/start"
	}
	if cmd, ok := commands[text]; ok {
		delete(b.dialogs, m.From.ID)
		return b.navigate(ctx, *m.From, u, cmd)
	}
	if d := b.dialogs[m.From.ID]; d != nil {
		return b.input(ctx, *m.From, u, text)
	}
	return b.send(ctx, m.Chat.ID, "Выберите раздел кнопками ниже.", admin)
}
func (b *Bot) HandleCallback(ctx context.Context, c *Callback) error {
	if c.Message == nil || c.Message.Chat.Type != "private" || c.From.ID != c.Message.Chat.ID {
		return nil
	}
	// Callback sender, never message.From (which is the bot), determines permissions.
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state(c.From.ID)
	_ = b.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": c.ID}, nil)
	u, e := b.account(ctx, c.From)
	if e != nil {
		if errors.Is(e, app.ErrTelegramAccount) {
			return b.send(ctx, c.From.ID, e.Error(), false)
		}
		return e
	}
	p := strings.Split(c.Data, ":")
	if len(p) == 0 {
		return nil
	}
	switch p[0] {
	case "nav":
		if len(p) == 2 {
			delete(b.dialogs, c.From.ID)
			return b.navigate(ctx, c.From, u, p[1])
		}
	case "choose":
		if u.Role == "admin" && len(p) == 3 {
			return b.studentPicker(ctx, c.From.ID, "filterset:"+map[string]string{"lessons": "schedule", "homework": "hw", "payments": "payments"}[p[1]]+":", int(number(p[2])), true)
		}
	case "period":
		if len(p) == 2 {
			return b.period(ctx, c.From, u, p[1])
		}
	case "filterset":
		if u.Role == "admin" && len(p) == 3 {
			b.views[c.From.ID].Student = number(p[2])
			return b.navigate(ctx, c.From, u, p[1])
		}
	case "list":
		if len(p) == 3 {
			return b.list(ctx, c.From, u, p[1], int(number(p[2])))
		}
	case "show":
		if len(p) == 3 {
			return b.detail(ctx, c.From, u, p[1], number(p[2]))
		}
	case "new":
		if u.Role == "admin" && len(p) == 2 {
			return b.begin(ctx, c.From, u, p[1], 0)
		}
	case "edit":
		if u.Role == "admin" && len(p) == 3 {
			return b.begin(ctx, c.From, u, p[1], number(p[2]))
		}
	case "submit":
		if u.Role == "student" && len(p) == 2 {
			return b.begin(ctx, c.From, u, "answer", number(p[1]))
		}
	case "form":
		return b.formCallback(ctx, c.From, u, p)
	}
	return b.send(ctx, c.From.ID, "Действие недоступно. Откройте раздел заново.", u.Role == "admin")
}
func (b *Bot) navigate(ctx context.Context, tg app.TelegramUser, u app.User, where string) error {
	switch where {
	case "schedule":
		return b.list(ctx, tg, u, "lessons", 0)
	case "today":
		return b.period(ctx, tg, u, "today")
	case "hw":
		return b.list(ctx, tg, u, "homework", 0)
	case "students":
		if u.Role == "admin" {
			return b.list(ctx, tg, u, "students", 0)
		}
	case "payments":
		if u.Role == "admin" {
			return b.list(ctx, tg, u, "payments", 0)
		}
	case "address":
		return b.screen(ctx, tg.ID, "Адрес проведения занятий\n\n"+app.LessonAddress, keyboard{row("Расписание", "nav:schedule")})
	case "app":
		if !b.miniAppAvailable() {
			return b.send(ctx, tg.ID, "Приложение станет доступно после настройки публичного HTTPS-адреса сайта в PUBLIC_URL. Расписание, ДЗ и остальные кнопки уже работают здесь.", u.Role == "admin")
		}
		return b.screen(ctx, tg.ID, "Откройте личный кабинет внутри Telegram.", keyboard{{button{"text": "Открыть приложение", "web_app": map[string]string{"url": b.App.Config.Origin + "/?telegram=1"}}}})
	}
	return b.send(ctx, tg.ID, "Вы вошли как "+u.Login+". Выберите раздел.", u.Role == "admin")
}
func (b *Bot) period(ctx context.Context, tg app.TelegramUser, u app.User, p string) error {
	v := b.state(tg.ID)
	n := time.Now().In(b.App.Config.Location)
	day := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location())
	switch p {
	case "today":
		v.Month = false
		v.From = day
		v.To = day.AddDate(0, 0, 1)
	case "week":
		v.Month = false
		v.From = day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
		v.To = v.From.AddDate(0, 0, 7)
	case "month":
		v.Month = true
		v.From = time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, n.Location())
		v.To = v.From.AddDate(0, 1, 0)
	case "prev":
		if v.Month {
			v.From = v.From.AddDate(0, -1, 0)
			v.To = v.From.AddDate(0, 1, 0)
		} else {
			d := v.To.Sub(v.From)
			v.From = v.From.Add(-d)
			v.To = v.To.Add(-d)
		}
	case "next":
		if v.Month {
			v.From = v.From.AddDate(0, 1, 0)
			v.To = v.From.AddDate(0, 1, 0)
		} else {
			d := v.To.Sub(v.From)
			v.From = v.From.Add(d)
			v.To = v.To.Add(d)
		}
	case "custom":
		return b.begin(ctx, tg, u, "range", 0)
	default:
		return nil
	}
	return b.list(ctx, tg, u, "lessons", 0)
}
func (b *Bot) studentPicker(ctx context.Context, id int64, prefix string, page int, all bool) error {
	ss, e := b.App.Students(ctx, 0)
	if e != nil {
		return e
	}
	if page < 0 {
		page = 0
	}
	start := page * 8
	if start > len(ss) {
		start = 0
		page = 0
	}
	end := start + 8
	if end > len(ss) {
		end = len(ss)
	}
	k := keyboard{}
	if all {
		k = append(k, row("Все ученики", prefix+"0"))
	}
	for _, s := range ss[start:end] {
		label := s.Name
		if !s.Active {
			label += " (архив)"
		}
		k = append(k, row(clip(label, 45), prefix+num(s.ID)))
	}
	// Picker pagination uses its own callback, never embeds free text in callback_data.
	if strings.HasPrefix(prefix, "form:") {
		base := strings.TrimSuffix(prefix, "value:")
		nav := []button{}
		if page > 0 {
			nav = append(nav, key("Назад", base+"pick:"+strconv.Itoa(page-1)))
		}
		if end < len(ss) {
			nav = append(nav, key("Далее", base+"pick:"+strconv.Itoa(page+1)))
		}
		if len(nav) > 0 {
			k = append(k, nav)
		}
	} else {
		parts := strings.Split(prefix, ":")
		target := map[string]string{"schedule": "lessons", "hw": "homework", "payments": "payments"}[parts[1]]
		nav := []button{}
		if page > 0 {
			nav = append(nav, key("Назад", "choose:"+target+":"+strconv.Itoa(page-1)))
		}
		if end < len(ss) {
			nav = append(nav, key("Далее", "choose:"+target+":"+strconv.Itoa(page+1)))
		}
		if len(nav) > 0 {
			k = append(k, nav)
		}
	}
	k = append(k, row("Меню", "nav:menu"))
	return b.screen(ctx, id, "Выберите ученика", k)
}
func pageBounds(length, page int) (int, int, int) {
	if page < 0 {
		page = 0
	}
	start := page * 8
	if start >= length {
		start = 0
		page = 0
	}
	end := start + 8
	if end > length {
		end = length
	}
	return start, end, page
}
func (b *Bot) list(ctx context.Context, tg app.TelegramUser, u app.User, kind string, page int) error {
	admin := u.Role == "admin"
	if !admin && kind != "lessons" && kind != "homework" {
		return b.navigate(ctx, tg, u, "menu")
	}
	v := b.state(tg.ID)
	scope := v.Student
	if !admin {
		scope = u.StudentID
	}
	k := keyboard{}
	text := ""
	items := []button{}
	switch kind {
	case "lessons":
		ls, e := b.App.Lessons(ctx, scope, v.From, v.To)
		if e != nil {
			return e
		}
		text = fmt.Sprintf("Расписание · %s\n%s — %s\n", b.App.Config.Location.String(), v.From.Format("02.01.2006"), v.To.Add(-time.Second).Format("02.01.2006"))
		if scope > 0 && admin {
			text += "Фильтр по ученику включен\n"
		}
		for _, l := range ls {
			items = append(items, key(clip(l.StartsAt.In(b.App.Config.Location).Format("02.01 15:04")+" · "+l.StudentName+" · "+lessonStatus(l.Status), 60), "show:lessons:"+num(l.ID)))
		}
		k = append(k, []button{key("Сегодня", "period:today"), key("Неделя", "period:week"), key("Месяц", "period:month")}, []button{key("Раньше", "period:prev"), key("Период", "period:custom"), key("Позже", "period:next")})
	case "homework":
		hs, e := b.App.HomeworkList(ctx, scope)
		if e != nil {
			return e
		}
		text = "Домашние задания · все задания\n"
		for _, h := range hs {
			items = append(items, key(clip(h.StudentName+" · "+h.Title, 55), "show:homework:"+num(h.ID)))
		}
	case "students":
		ss, e := b.App.Students(ctx, 0)
		if e != nil {
			return e
		}
		text = "Ученики\n"
		for _, s := range ss {
			label := s.Name
			if !s.Active {
				label += " (архив)"
			}
			items = append(items, key(clip(label, 55), "show:students:"+num(s.ID)))
		}
	case "payments":
		ps, e := b.App.Payments(ctx, scope)
		if e != nil {
			return e
		}
		text = fmt.Sprintf("Доход и оплаты\nПериод: %s — %s\n", v.From.Format("02.01.2006"), v.To.Add(-time.Second).Format("02.01.2006"))
		var sum int64
		for _, p := range ps {
			if p.PaidAt.Before(v.From) || !p.PaidAt.Before(v.To) {
				continue
			}
			if p.VoidReason == "" {
				sum += p.Amount
			}
			label := p.PaidAt.In(b.App.Config.Location).Format("02.01") + " · " + p.StudentName + " · " + app.Money(p.Amount)
			if p.VoidReason != "" {
				label += " (отменен)"
			}
			items = append(items, key(clip(label, 60), "show:payments:"+num(p.ID)))
		}
		ls, e := b.App.Lessons(ctx, scope, v.From, v.To)
		if e != nil {
			return e
		}
		var earned int64
		for _, l := range ls {
			if l.Status == "completed" {
				earned += l.Price
			}
		}
		ss, e := b.App.Students(ctx, scope)
		if e != nil {
			return e
		}
		var debt, advance int64
		for _, s := range ss {
			balance := s.Paid - s.Charges
			if balance < 0 {
				debt -= balance
			} else {
				advance += balance
			}
		}
		text += fmt.Sprintf("Получено за период: %s\nПроведено занятий на: %s\nОбщий долг: %s\nОбщий аванс: %s\n", app.Money(sum), app.Money(earned), app.Money(debt), app.Money(advance))
		k = append(k, row("Изменить период", "new:income-range"))
	default:
		return nil
	}
	start, end, page := pageBounds(len(items), page)
	text += fmt.Sprintf("\nЗаписей: %d", len(items))
	if len(items) > 0 {
		text += fmt.Sprintf(" · страница %d", page+1)
	}
	for _, item := range items[start:end] {
		k = append(k, []button{item})
	}
	nav := []button{}
	if page > 0 {
		nav = append(nav, key("Предыдущие", "list:"+kind+":"+strconv.Itoa(page-1)))
	}
	if end < len(items) {
		nav = append(nav, key("Следующие", "list:"+kind+":"+strconv.Itoa(page+1)))
	}
	if len(nav) > 0 {
		k = append(k, nav)
	}
	if admin {
		if kind != "students" {
			k = append(k, row("Выбрать ученика", "choose:"+kind+":0"))
		}
		label := map[string]string{"students": "Добавить ученика", "lessons": "Добавить занятие", "homework": "Выдать ДЗ", "payments": "Записать оплату"}[kind]
		k = append(k, row(label, "new:"+kind))
	}
	k = append(k, row("Меню", "nav:menu"))
	return b.screen(ctx, tg.ID, text, k)
}
func (b *Bot) detail(ctx context.Context, tg app.TelegramUser, u app.User, kind string, id int64) error {
	admin := u.Role == "admin"
	scope := u.StudentID
	if admin {
		scope = 0
	}
	k := keyboard{}
	text := ""
	switch kind {
	case "students":
		if !admin {
			return nil
		}
		ss, e := b.App.Students(ctx, id)
		if e != nil {
			return e
		}
		if len(ss) == 0 {
			return b.send(ctx, tg.ID, "Ученик не найден", true)
		}
		s := ss[0]
		text = fmt.Sprintf("%s\nЛогин / Telegram: %s\nТелефон: %s\nАктивен: %t\nОплачено: %s\nНачислено: %s\nЗаметки: %s", s.Name, s.Login, s.Phone, s.Active, app.Money(s.Paid), app.Money(s.Charges), s.Notes)
		k = append(k, row("Расписание ученика", "filterset:schedule:"+num(id)), row("ДЗ ученика", "filterset:hw:"+num(id)), row("Оплаты ученика", "filterset:payments:"+num(id)), row("Сбросить пароль", "edit:password:"+num(id)), row("Отключить Telegram", "edit:unlink:"+num(id)))
	case "lessons":
		var l app.Lesson
		e := b.App.DB.QueryRowContext(ctx, `SELECT l.id,l.student_id,s.name,l.title,l.starts_at,l.ends_at,l.location,l.price_kop,l.status,l.notes FROM lessons l JOIN students s ON s.id=l.student_id WHERE l.id=$1 AND ($2::bigint=0 OR l.student_id=$2)`, id, scope).Scan(&l.ID, &l.StudentID, &l.StudentName, &l.Title, &l.StartsAt, &l.EndsAt, &l.Location, &l.Price, &l.Status, &l.Notes)
		if e != nil {
			return b.send(ctx, tg.ID, "Занятие недоступно", admin)
		}
		text = fmt.Sprintf("%s\n%s\n%s — %s\n%s\n\n%s", l.StudentName, l.Title, l.StartsAt.In(b.App.Config.Location).Format("02.01.2006 15:04"), l.EndsAt.In(b.App.Config.Location).Format("15:04"), lessonStatus(l.Status), app.LessonAddress)
		if admin {
			text += "\nСтоимость: " + app.Money(l.Price) + "\nЗаметки: " + l.Notes
		}
	case "homework":
		hs, e := b.App.HomeworkList(ctx, scope)
		if e != nil {
			return e
		}
		found := false
		for _, h := range hs {
			if h.ID != id {
				continue
			}
			found = true
			text = fmt.Sprintf("%s · %s\nСрок: %s\n\n%s\n%s\n\nОтвет: %s\n%s\nКомментарий: %s", h.StudentName, h.Title, h.DueAt.In(b.App.Config.Location).Format("02.01.2006 15:04"), h.Body, h.ResourceURL, h.Answer, h.AnswerURL, h.Feedback)
			if !admin && h.Status != "reviewed" {
				k = append(k, row("Отправить ответ", "submit:"+num(id)))
			}
		}
		if !found {
			return b.send(ctx, tg.ID, "Задание недоступно", admin)
		}
	case "payments":
		if !admin {
			return nil
		}
		ps, e := b.App.Payments(ctx, 0)
		if e != nil {
			return e
		}
		found := false
		for _, p := range ps {
			if p.ID != id {
				continue
			}
			found = true
			text = fmt.Sprintf("%s\n%s · %s\nСпособ: %s\n%s\nПричина отмены: %s", p.StudentName, app.Money(p.Amount), p.PaidAt.In(b.App.Config.Location).Format("02.01.2006 15:04"), display(p.Method), p.Note, p.VoidReason)
			if p.VoidReason == "" {
				k = append(k, row("Отменить платеж", "edit:void:"+num(id)))
			}
		}
		if !found {
			return b.send(ctx, tg.ID, "Платеж не найден", true)
		}
	default:
		return nil
	}
	if admin && kind != "payments" {
		k = append(k, row("Изменить", "edit:"+kind+":"+num(id)))
	}
	k = append(k, row("К списку", "list:"+kind+":0"), row("Меню", "nav:menu"))
	return b.screen(ctx, tg.ID, text, k)
}
func display(s string) string {
	if v, ok := map[string]string{"planned": "Запланировано", "completed": "Проведено", "cancelled": "Отменено", "missed": "Пропуск", "assigned": "Выдано", "submitted": "На проверке", "reviewed": "Проверено", "cash": "Наличные", "transfer": "Перевод", "other": "Другое", "true": "Активен", "false": "В архиве"}[s]; ok {
		return v
	}
	return s
}

func (b *Bot) miniAppAvailable() bool {
	u, e := url.Parse(b.App.Config.Origin)
	if e != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return u.Scheme == "https" && host != "" && host != "localhost" && host != "127.0.0.1" && host != "::1"
}

func (b *Bot) configureMenu(ctx context.Context) error {
	if !b.miniAppAvailable() {
		return b.call(ctx, "setChatMenuButton", map[string]any{"menu_button": map[string]any{"type": "commands"}}, nil)
	}
	return b.call(ctx, "setChatMenuButton", map[string]any{"menu_button": map[string]any{"type": "web_app", "text": "Приложение", "web_app": map[string]string{"url": b.App.Config.Origin + "/?telegram=1"}}}, nil)
}
