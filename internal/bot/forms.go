package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"artem-study/internal/app"
)

func (b *Bot) begin(ctx context.Context, tg app.TelegramUser, u app.User, kind string, id int64) error {
	if u.Role != "admin" && kind != "range" && kind != "answer" {
		return nil
	}
	nonce, e := app.Token()
	if e != nil {
		return e
	}
	d := &dialog{Kind: kind, ID: id, Nonce: nonce[:16], Data: map[string]any{}, Until: time.Now().Add(30 * time.Minute), Step: 0, Scope: u.StudentID}
	now := time.Now().In(b.App.Config.Location)
	switch kind {
	case "students":
		d.Data = map[string]any{"subject": "Музыка", "active": true, "phone": "", "notes": ""}
		d.Fields = []field{f("name", "Имя и фамилия", "text"), f("login", "Telegram username без @ (он же логин)", "login"), optional("phone", "Телефон", "text"), optional("notes", "Заметки преподавателя", "text")}
		if id == 0 {
			d.Fields = append(d.Fields, f("password", "Пароль для входа на сайт", "password"))
		} else {
			ss, e := b.App.Students(ctx, id)
			if e != nil {
				return e
			}
			if len(ss) == 0 {
				return b.send(ctx, tg.ID, "Ученик не найден", true)
			}
			s := ss[0]
			d.Data = map[string]any{"name": s.Name, "login": s.Login, "phone": s.Phone, "notes": s.Notes, "subject": s.Subject, "active": s.Active}
			d.Fields = append(d.Fields, enum("active", "Статус ученика", opt("Активен", "true"), opt("В архиве", "false")))
		}
	case "lessons":
		d.Data = map[string]any{"location": app.LessonAddress, "status": "planned", "notes": "", "repeat": 1}
		d.Fields = []field{f("student_id", "Ученик", "student"), f("title", "Название занятия", "text"), f("starts_at", "Начало занятия", "datetime"), f("ends_at", "Конец занятия", "datetime"), f("price_kop", "Стоимость в рублях", "money"), optional("notes", "Заметки преподавателя", "text")}
		if id == 0 {
			d.Fields = append(d.Fields, f("repeat", "Количество еженедельных занятий (1–12)", "repeat"))
		} else {
			var l app.Lesson
			e := b.App.DB.QueryRowContext(ctx, `SELECT student_id,title,starts_at,ends_at,price_kop,status,notes FROM lessons WHERE id=$1`, id).Scan(&l.StudentID, &l.Title, &l.StartsAt, &l.EndsAt, &l.Price, &l.Status, &l.Notes)
			if e != nil {
				return b.send(ctx, tg.ID, "Занятие не найдено", true)
			}
			d.Data = map[string]any{"student_id": l.StudentID, "title": l.Title, "starts_at": l.StartsAt.Format(time.RFC3339), "ends_at": l.EndsAt.Format(time.RFC3339), "price_kop": l.Price, "status": l.Status, "notes": l.Notes, "location": app.LessonAddress, "repeat": 1}
			d.Fields = append(d.Fields, enum("status", "Статус занятия", opt("Запланировано", "planned"), opt("Проведено", "completed"), opt("Отменено", "cancelled"), opt("Пропуск", "missed")))
		}
	case "homework":
		d.Data = map[string]any{"status": "assigned", "feedback": "", "resource_url": ""}
		d.Fields = []field{f("student_id", "Ученик", "student"), f("title", "Название ДЗ", "text"), f("body", "Текст задания", "text"), f("due_at", "Срок выполнения", "datetime"), optional("resource_url", "Ссылка на материал", "url")}
		if id > 0 {
			hs, e := b.App.HomeworkList(ctx, 0)
			if e != nil {
				return e
			}
			found := false
			for _, h := range hs {
				if h.ID == id {
					found = true
					d.Data = map[string]any{"student_id": h.StudentID, "title": h.Title, "body": h.Body, "due_at": h.DueAt.Format(time.RFC3339), "resource_url": h.ResourceURL, "status": h.Status, "feedback": h.Feedback}
				}
			}
			if !found {
				return b.send(ctx, tg.ID, "ДЗ не найдено", true)
			}
			d.Fields = append(d.Fields[1:], optional("feedback", "Комментарий преподавателя", "text"))
		}
	case "payments":
		token, e := app.Token()
		if e != nil {
			return e
		}
		d.Data = map[string]any{"request_id": token, "paid_at": now.Format(time.RFC3339), "note": ""}
		d.Fields = []field{f("student_id", "Ученик", "student"), f("amount_kop", "Сумма в рублях", "money"), f("paid_at", "Дата и время оплаты", "datetime"), enum("method", "Способ оплаты", opt("Наличные", "cash"), opt("Перевод", "transfer"), opt("Другое", "other")), optional("note", "Комментарий к оплате", "text")}
	case "password":
		d.Fields = []field{f("password", "Новый пароль ученика", "password")}
	case "void":
		d.Fields = []field{f("reason", "Причина отмены платежа", "text")}
	case "unlink":
		d.Data["student_id"] = id
		d.Fields = []field{}
	case "range", "income-range":
		d.Fields = []field{f("from", "Начало периода", "date"), f("to", "Последний день периода включительно", "date")}
	case "answer":
		hs, e := b.App.HomeworkList(ctx, u.StudentID)
		if e != nil {
			return e
		}
		found := false
		for _, h := range hs {
			if h.ID == id && h.Status != "reviewed" {
				found = true
				d.Data = map[string]any{"answer": h.Answer, "answer_url": h.AnswerURL}
			}
		}
		if !found {
			return b.send(ctx, tg.ID, "Задание недоступно для ответа", false)
		}
		d.Fields = []field{optional("answer", "Текст ответа", "text"), optional("answer_url", "Ссылка на запись или файл", "url")}
	default:
		return nil
	}
	if id > 0 && kind != "password" && kind != "void" && kind != "answer" {
		d.Editing = true
		d.Step = -1
	}
	b.dialogs[tg.ID] = d
	return b.prompt(ctx, tg.ID, d)
}
func (b *Bot) prompt(ctx context.Context, id int64, d *dialog) error {
	d.Until = time.Now().Add(30 * time.Minute)
	if d.Step < 0 || d.Step >= len(d.Fields) {
		return b.review(ctx, id, d)
	}
	field := d.Fields[d.Step]
	base := "form:" + d.Nonce + ":" + strconv.Itoa(d.Step) + ":"
	if field.Type == "student" {
		return b.studentPicker(ctx, id, base+"value:", 0, false)
	}
	text := field.Label
	if field.Type == "datetime" {
		text += "\nВведите ДД.ММ.ГГГГ ЧЧ:ММ. Часовой пояс: " + b.App.Config.Location.String()
	}
	if field.Type == "date" {
		text += "\nВведите ДД.ММ.ГГГГ"
	}
	if field.Type == "password" {
		text += "\nПароль будет скрыт в итоговой карточке."
	}
	k := keyboard{}
	for _, o := range field.Options {
		k = append(k, row(o.Label, base+"value:"+o.Value))
	}
	if field.Optional {
		k = append(k, row("Оставить пустым", base+"empty"))
	}
	if d.Data[field.Key] != nil && field.Type != "password" {
		text += "\nТекущее значение: " + b.fieldText(field, d.Data[field.Key])
		k = append(k, row("Оставить текущее", base+"keep"))
	}
	if field.Type == "datetime" {
		k = append(k, row("Сейчас", base+"now"))
	}
	k = append(k, row("Отмена", "nav:menu"))
	return b.screen(ctx, id, text, k)
}
func (b *Bot) fieldText(f field, v any) string {
	if v == nil {
		return "не заполнено"
	}
	if f.Type == "password" {
		return "задан"
	}
	raw := fmt.Sprint(v)
	if f.Type == "money" {
		return app.Money(number(raw))
	}
	if f.Type == "datetime" {
		if t, e := time.Parse(time.RFC3339, raw); e == nil {
			return t.In(b.App.Config.Location).Format("02.01.2006 15:04")
		}
	}
	return clip(display(raw), 350)
}
func (b *Bot) review(ctx context.Context, id int64, d *dialog) error {
	d.Step = -1
	text := "Проверьте данные перед сохранением\n"
	k := keyboard{}
	for i, f := range d.Fields {
		text += "\n" + f.Label + ": " + b.fieldText(f, d.Data[f.Key])
		k = append(k, row("Изменить: "+f.Label, "form:"+d.Nonce+":-1:edit:"+strconv.Itoa(i)))
	}
	if sid := number(fmt.Sprint(d.Data["student_id"])); sid > 0 {
		ss, e := b.App.Students(ctx, sid)
		if e != nil {
			return e
		}
		if len(ss) > 0 {
			text += "\nУченик: " + ss[0].Name
		}
	}
	if d.Kind == "unlink" {
		text = "Отключить Telegram ученика и завершить его сеансы? После этого новый Telegram сможет подключиться по совпадающему логину."
	}
	if d.Kind == "lessons" {
		text += "\nАдрес: " + app.LessonAddress
	}
	k = append(k, row("Подтвердить и сохранить", "form:"+d.Nonce+":-1:save"), row("Отмена", "nav:menu"))
	return b.screen(ctx, id, text, k)
}
func (b *Bot) formCallback(ctx context.Context, tg app.TelegramUser, u app.User, p []string) error {
	d := b.dialogs[tg.ID]
	if len(p) < 4 || d == nil || p[1] != d.Nonce || p[2] != strconv.Itoa(d.Step) {
		return b.send(ctx, tg.ID, "Эта форма уже закрыта или шаг изменился. Используйте последнее сообщение бота.", u.Role == "admin")
	}
	switch p[3] {
	case "edit":
		if d.Step == -1 && len(p) == 5 {
			i := int(number(p[4]))
			if i >= 0 && i < len(d.Fields) {
				d.Editing = true
				d.Step = i
				return b.prompt(ctx, tg.ID, d)
			}
		}
	case "save":
		if d.Step == -1 {
			return b.save(ctx, tg, u, d)
		}
	case "pick":
		if d.Step >= 0 && d.Fields[d.Step].Type == "student" && len(p) == 5 {
			return b.studentPicker(ctx, tg.ID, "form:"+d.Nonce+":"+p[2]+":value:", int(number(p[4])), false)
		}
	case "value":
		if d.Step >= 0 && len(p) == 5 {
			return b.input(ctx, tg, u, p[4])
		}
	case "empty":
		if d.Step >= 0 && d.Fields[d.Step].Optional {
			return b.input(ctx, tg, u, "")
		}
	case "now":
		if d.Step >= 0 && d.Fields[d.Step].Type == "datetime" {
			return b.input(ctx, tg, u, time.Now().In(b.App.Config.Location).Format("02.01.2006 15:04"))
		}
	case "keep":
		if d.Step >= 0 && d.Data[d.Fields[d.Step].Key] != nil {
			return b.advance(ctx, tg.ID, d)
		}
	}
	return nil
}
func parseMoney(s string) (int64, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", ".")
	p := strings.Split(s, ".")
	if len(p) > 2 || len(p[0]) == 0 {
		return 0, fmt.Errorf("Введите сумму, например 1500 или 1500,50")
	}
	for _, v := range p {
		for _, r := range v {
			if r < '0' || r > '9' {
				return 0, fmt.Errorf("Сумма должна быть положительным числом")
			}
		}
	}
	whole, e := strconv.ParseInt(p[0], 10, 64)
	if e != nil || whole > 1000000 {
		return 0, fmt.Errorf("Максимум 1 000 000 руб.")
	}
	var cents int64
	if len(p) == 2 {
		if len(p[1]) > 2 {
			return 0, fmt.Errorf("Не более двух цифр после запятой")
		}
		fraction := p[1] + strings.Repeat("0", 2-len(p[1]))
		cents, _ = strconv.ParseInt(fraction, 10, 64)
	}
	result := whole*100 + cents
	if result > 100000000 {
		return 0, fmt.Errorf("Максимум 1 000 000 руб.")
	}
	return result, nil
}
func (b *Bot) input(ctx context.Context, tg app.TelegramUser, u app.User, text string) error {
	d := b.dialogs[tg.ID]
	if d == nil {
		return nil
	}
	if d.Step < 0 {
		return b.review(ctx, tg.ID, d)
	}
	field := d.Fields[d.Step]
	var value any = text
	var err error
	if !field.Optional && strings.TrimSpace(text) == "" {
		err = fmt.Errorf("Поле не может быть пустым")
	}
	if len(text) > 12000 {
		err = fmt.Errorf("Текст слишком длинный")
	}
	if err == nil {
		switch field.Type {
		case "login":
			text = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(text), "@"))
			if !app.ValidLogin(text) {
				err = fmt.Errorf("Укажите username без @, от 3 до 64 символов")
			}
			value = text
		case "password":
			if len(text) == 0 || len(text) > 72 {
				err = fmt.Errorf("Пароль должен быть непустым, не более 72 байт")
			}
		case "url":
			if !app.ValidURL(text) {
				err = fmt.Errorf("Ссылка должна начинаться с https:// или http://")
			}
		case "money":
			value, err = parseMoney(text)
		case "repeat":
			n, e := strconv.Atoi(text)
			if e != nil || n < 1 || n > 12 {
				err = fmt.Errorf("Введите число от 1 до 12")
			}
			value = n
		case "student":
			id := number(text)
			if id <= 0 {
				err = fmt.Errorf("Выберите ученика кнопкой")
			} else {
				ss, e := b.App.Students(ctx, id)
				if e != nil {
					return e
				}
				if len(ss) == 0 || !ss[0].Active {
					err = fmt.Errorf("Выберите активного ученика")
				}
			}
			value = id
		case "datetime", "date":
			layout := "02.01.2006 15:04"
			if field.Type == "date" {
				layout = "02.01.2006"
			}
			t, e := time.ParseInLocation(layout, text, b.App.Config.Location)
			if e != nil {
				err = fmt.Errorf("Неверная дата. Формат: %s", layout)
			} else {
				value = t.Format(time.RFC3339)
			}
		case "enum":
			valid := false
			for _, o := range field.Options {
				if text == o.Value || text == o.Label {
					value = o.Value
					valid = true
				}
			}
			if !valid {
				err = fmt.Errorf("Выберите значение кнопкой")
			}
			if field.Key == "active" {
				value = fmt.Sprint(value) == "true"
			}
		}
	}
	if err != nil {
		return b.screen(ctx, tg.ID, err.Error(), keyboard{row("Отмена", "nav:menu")})
	}
	d.Data[field.Key] = value
	return b.advance(ctx, tg.ID, d)
}
func (b *Bot) advance(ctx context.Context, id int64, d *dialog) error {
	if d.Editing {
		d.Step = -1
	} else {
		d.Step++
	}
	return b.prompt(ctx, id, d)
}
func (b *Bot) save(ctx context.Context, tg app.TelegramUser, u app.User, d *dialog) error {
	if u.Role != "admin" && d.Kind != "range" && d.Kind != "answer" {
		return nil
	}
	if d.Kind == "range" || d.Kind == "income-range" {
		from, e1 := time.Parse(time.RFC3339, fmt.Sprint(d.Data["from"]))
		end, e2 := time.Parse(time.RFC3339, fmt.Sprint(d.Data["to"]))
		to := end.AddDate(0, 0, 1)
		if e1 != nil || e2 != nil || !to.After(from) || to.Sub(from) > 370*24*time.Hour {
			return b.screen(ctx, tg.ID, "Выберите корректный период до одного года. Измените даты в карточке выше.", keyboard{row("Исправить", "form:"+d.Nonce+":-1:edit:0")})
		}
		v := b.state(tg.ID)
		v.Month = false
		v.From = from
		v.To = to
		delete(b.dialogs, tg.ID)
		kind := "lessons"
		if d.Kind == "income-range" {
			kind = "payments"
		}
		return b.list(ctx, tg, u, kind, 0)
	}
	method := "POST"
	path := "/" + d.Kind
	if d.ID > 0 {
		method = "PUT"
		path += "/" + num(d.ID)
	}
	switch d.Kind {
	case "password":
		method = "POST"
		path = "/students/" + num(d.ID) + "/password"
	case "void":
		method = "POST"
		path = "/payments/" + num(d.ID) + "/void"
	case "unlink":
		method = "POST"
		path = "/telegram/unlink"
	case "answer":
		method = "POST"
		path = "/homework/" + num(d.ID) + "/submit"
	}
	// Single-use confirmation before writing; retrying a callback must not duplicate a payment/lesson.
	delete(b.dialogs, tg.ID)
	raw, e := b.App.BotRequest(ctx, tg, method, path, d.Data)
	if e != nil {
		b.dialogs[tg.ID] = d
		token, tokenErr := app.Token()
		if tokenErr != nil {
			delete(b.dialogs, tg.ID)
			return tokenErr
		}
		d.Nonce = token[:16]
		if err := b.send(ctx, tg.ID, e.Error(), u.Role == "admin"); err != nil {
			return err
		}
		return b.review(ctx, tg.ID, d)
	}
	var result struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(raw, &result)
	if e = b.send(ctx, tg.ID, "Сохранено.", u.Role == "admin"); e != nil {
		return e
	}
	switch d.Kind {
	case "students", "lessons", "homework", "payments":
		if result.ID > 0 {
			return b.detail(ctx, tg, u, d.Kind, result.ID)
		}
	case "password", "unlink":
		return b.detail(ctx, tg, u, "students", d.ID)
	case "void":
		return b.detail(ctx, tg, u, "payments", d.ID)
	case "answer":
		return b.detail(ctx, tg, u, "homework", d.ID)
	}
	return nil
}
