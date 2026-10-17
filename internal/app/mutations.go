package app

import (
	"database/sql"
	"net/http"
	"strings"
	"time"
)

func short(s string, n int) bool { return len(strings.TrimSpace(s)) > 0 && len(s) <= n }
func (api *API) saveStudent(w http.ResponseWriter, r *http.Request, id int64) {
	var in struct {
		Name     string `json:"name"`
		Phone    string `json:"phone"`
		Subject  string `json:"subject"`
		Notes    string `json:"notes"`
		Active   bool   `json:"active"`
		Login    string `json:"login"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Login = strings.ToLower(strings.TrimSpace(in.Login))
	in.Name = strings.TrimSpace(in.Name)
	if !short(in.Name, 200) || !short(in.Subject, 200) || len(in.Phone) > 100 || len(in.Notes) > 5000 || !ValidLogin(in.Login) {
		fail(w, 400, "Проверьте имя, предмет и логин (латиница, 3-64 символа)")
		return
	}
	var hash string
	var e error
	if id == 0 {
		hash, e = PasswordHash(in.Password)
		if e != nil {
			fail(w, 400, e.Error())
			return
		}
	}
	tx, e := api.DB.BeginTx(r.Context(), nil)
	if e != nil {
		dbFail(w, e)
		return
	}
	defer tx.Rollback()
	if id == 0 {
		e = tx.QueryRowContext(r.Context(), "INSERT INTO students(name,phone,subject,notes,active) VALUES($1,$2,$3,$4,true) RETURNING id", in.Name, in.Phone, in.Subject, in.Notes).Scan(&id)
		if e == nil {
			_, e = tx.ExecContext(r.Context(), "INSERT INTO users(login,password_hash,role,student_id) VALUES($1,$2,'student',$3)", in.Login, hash, id)
		}
	} else {
		e = tx.QueryRowContext(r.Context(), "UPDATE students SET name=$1,phone=$2,subject=$3,notes=$4,active=$5 WHERE id=$6 RETURNING id", in.Name, in.Phone, in.Subject, in.Notes, in.Active, id).Scan(&id)
		if e == nil {
			_, e = tx.ExecContext(r.Context(), "UPDATE users SET login=$1 WHERE student_id=$2", in.Login, id)
		}
		if e == nil && !in.Active {
			_, e = tx.ExecContext(r.Context(), "DELETE FROM sessions WHERE user_id=(SELECT id FROM users WHERE student_id=$1)", id)
		}
		if e == nil && !in.Active {
			_, e = tx.ExecContext(r.Context(), "DELETE FROM telegram_links WHERE student_id=$1", id)
		}
		if e == nil && !in.Active {
			_, e = tx.ExecContext(r.Context(), "DELETE FROM telegram_invites WHERE student_id=$1", id)
		}
	}
	if e != nil {
		dbFail(w, e)
		return
	}
	if e = tx.Commit(); e != nil {
		dbFail(w, e)
		return
	}
	writeJSON(w, 200, map[string]int64{"id": id})
}
func (api *API) saveLesson(w http.ResponseWriter, r *http.Request, id int64) {
	var in struct {
		StudentID int64     `json:"student_id"`
		Title     string    `json:"title"`
		StartsAt  time.Time `json:"starts_at"`
		EndsAt    time.Time `json:"ends_at"`
		Location  string    `json:"location"`
		Price     int64     `json:"price_kop"`
		Status    string    `json:"status"`
		Notes     string    `json:"notes"`
		Repeat    int       `json:"repeat"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !short(in.Title, 200) || !short(in.Location, 500) || len(in.Notes) > 5000 || in.StudentID < 1 || in.StartsAt.IsZero() || in.EndsAt.Sub(in.StartsAt) < 5*time.Minute || in.EndsAt.Sub(in.StartsAt) > 12*time.Hour || in.Price < 0 || in.Price > 100000000 || !oneOf(in.Status, "planned", "completed", "cancelled", "missed") {
		fail(w, 400, "Проверьте занятие: длительность от 5 минут до 12 часов, стоимость от 0 до 1 000 000 руб.")
		return
	}
	if in.Status == "completed" && in.EndsAt.After(time.Now()) {
		fail(w, 400, "Будущее занятие еще нельзя отметить проведенным")
		return
	}
	if in.Repeat == 0 {
		in.Repeat = 1
	}
	if in.Repeat < 1 || in.Repeat > 12 || id > 0 && in.Repeat != 1 || in.Repeat > 1 && in.Status != "planned" {
		fail(w, 400, "Повтор возможен для нового запланированного занятия, от 1 до 12 недель")
		return
	}
	tx, e := api.DB.BeginTx(r.Context(), nil)
	if e != nil {
		dbFail(w, e)
		return
	}
	defer tx.Rollback()
	var active bool
	if e = tx.QueryRowContext(r.Context(), "SELECT active FROM students WHERE id=$1 FOR UPDATE", in.StudentID).Scan(&active); e != nil {
		dbFail(w, e)
		return
	}
	if !active && id == 0 {
		fail(w, 400, "Ученик находится в архиве")
		return
	}
	if id > 0 {
		e = tx.QueryRowContext(r.Context(), "UPDATE lessons SET student_id=$1,title=$2,starts_at=$3,ends_at=$4,location=$5,price_kop=$6,status=$7,notes=$8 WHERE id=$9 RETURNING id", in.StudentID, in.Title, in.StartsAt, in.EndsAt, in.Location, in.Price, in.Status, in.Notes, id).Scan(&id)
	} else {
		for i := 0; i < in.Repeat; i++ {
			start := in.StartsAt.In(api.Config.Location).AddDate(0, 0, 7*i)
			end := start.Add(in.EndsAt.Sub(in.StartsAt))
			e = tx.QueryRowContext(r.Context(), "INSERT INTO lessons(student_id,title,starts_at,ends_at,location,price_kop,status,notes) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id", in.StudentID, in.Title, start, end, in.Location, in.Price, in.Status, in.Notes).Scan(&id)
			if e != nil {
				break
			}
		}
	}
	if e != nil {
		dbFail(w, e)
		return
	}
	if e = tx.Commit(); e != nil {
		dbFail(w, e)
		return
	}
	writeJSON(w, 200, map[string]int64{"id": id})
}
func oneOf(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}
func (api *API) saveHomework(w http.ResponseWriter, r *http.Request, id int64) {
	var in struct {
		StudentID   int64     `json:"student_id"`
		Title       string    `json:"title"`
		Body        string    `json:"body"`
		ResourceURL string    `json:"resource_url"`
		DueAt       time.Time `json:"due_at"`
		Status      string    `json:"status"`
		Feedback    string    `json:"feedback"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !short(in.Title, 200) || !short(in.Body, 12000) || !ValidURL(in.ResourceURL) || in.DueAt.IsZero() || in.StudentID < 1 || len(in.Feedback) > 6000 || !oneOf(in.Status, "assigned", "submitted", "reviewed") {
		fail(w, 400, "Заполните название, задание и срок. Ссылки должны начинаться с http:// или https://")
		return
	}
	tx, e := api.DB.BeginTx(r.Context(), nil)
	if e != nil {
		dbFail(w, e)
		return
	}
	defer tx.Rollback()
	if id == 0 {
		if in.Status != "assigned" {
			fail(w, 400, "Новое ДЗ должно иметь статус «Выдано»")
			return
		}
		var active bool
		if e = tx.QueryRowContext(r.Context(), "SELECT active FROM students WHERE id=$1 FOR UPDATE", in.StudentID).Scan(&active); e != nil {
			dbFail(w, e)
			return
		}
		if !active {
			fail(w, 400, "Ученик находится в архиве")
			return
		}
		e = tx.QueryRowContext(r.Context(), "INSERT INTO homework(student_id,title,body,resource_url,due_at) VALUES($1,$2,$3,$4,$5) RETURNING id", in.StudentID, in.Title, in.Body, in.ResourceURL, in.DueAt).Scan(&id)
	} else {
		// Do not transfer an existing student's answer to another student.
		e = tx.QueryRowContext(r.Context(), "UPDATE homework SET title=$1,body=$2,resource_url=$3,due_at=$4,status=$5,feedback=$6 WHERE id=$7 AND student_id=$8 RETURNING id", in.Title, in.Body, in.ResourceURL, in.DueAt, in.Status, in.Feedback, id, in.StudentID).Scan(&id)
	}
	if e != nil {
		dbFail(w, e)
		return
	}
	if e = tx.Commit(); e != nil {
		dbFail(w, e)
		return
	}
	writeJSON(w, 200, map[string]int64{"id": id})
}
func (api *API) submit(w http.ResponseWriter, r *http.Request, id int64, u User) {
	var in struct {
		Answer string `json:"answer"`
		URL    string `json:"answer_url"`
	}
	if !decode(w, r, &in) {
		return
	}
	if (strings.TrimSpace(in.Answer) == "" && in.URL == "") || len(in.Answer) > 12000 || !ValidURL(in.URL) {
		fail(w, 400, "Добавьте ответ или ссылку на запись")
		return
	}
	e := api.DB.QueryRowContext(r.Context(), "UPDATE homework SET answer=$1,answer_url=$2,status='submitted',feedback='' WHERE id=$3 AND student_id=$4 AND status IN ('assigned','submitted') RETURNING id", in.Answer, in.URL, id, u.StudentID).Scan(&id)
	if e != nil {
		dbFail(w, e)
		return
	}
	writeJSON(w, 200, map[string]int64{"id": id})
}
func (api *API) addPayment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		StudentID int64     `json:"student_id"`
		Amount    int64     `json:"amount_kop"`
		PaidAt    time.Time `json:"paid_at"`
		Method    string    `json:"method"`
		Note      string    `json:"note"`
		RequestID string    `json:"request_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.StudentID < 1 || in.Amount < 1 || in.Amount > 100000000 || in.PaidAt.IsZero() || in.PaidAt.After(time.Now().Add(time.Minute)) || !oneOf(in.Method, "cash", "transfer", "other") || len(in.Note) > 2000 || len(in.RequestID) < 16 || len(in.RequestID) > 128 {
		fail(w, 400, "Проверьте сумму, дату и способ оплаты")
		return
	}
	var id int64
	e := api.DB.QueryRowContext(r.Context(), `INSERT INTO payments(student_id,amount_kop,paid_at,method,note,request_id) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(request_id) DO UPDATE SET request_id=EXCLUDED.request_id WHERE payments.student_id=EXCLUDED.student_id AND payments.amount_kop=EXCLUDED.amount_kop AND payments.paid_at=EXCLUDED.paid_at AND payments.method=EXCLUDED.method AND payments.note=EXCLUDED.note RETURNING id`, in.StudentID, in.Amount, in.PaidAt, in.Method, in.Note, in.RequestID).Scan(&id)
	if e == sql.ErrNoRows {
		fail(w, 409, "Этот платеж уже сохранен с другими данными. Обновите список оплат.")
		return
	}
	if e != nil {
		dbFail(w, e)
		return
	}
	writeJSON(w, 200, map[string]int64{"id": id})
}
func (api *API) voidPayment(w http.ResponseWriter, r *http.Request, id int64) {
	var in struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !short(in.Reason, 1000) {
		fail(w, 400, "Укажите причину отмены")
		return
	}
	e := api.DB.QueryRowContext(r.Context(), "UPDATE payments SET void_reason=$1 WHERE id=$2 AND void_reason='' RETURNING id", in.Reason, id).Scan(&id)
	if e != nil {
		dbFail(w, e)
		return
	}
	writeJSON(w, 200, map[string]int64{"id": id})
}
func (api *API) invite(w http.ResponseWriter, r *http.Request, u User) {
	var in struct {
		StudentID int64 `json:"student_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if u.Role == "student" {
		in.StudentID = u.StudentID
	}
	if in.StudentID < 1 {
		fail(w, 400, "Выберите ученика")
		return
	}
	tx, e := api.DB.BeginTx(r.Context(), nil)
	if e != nil {
		dbFail(w, e)
		return
	}
	defer tx.Rollback()
	var active bool
	if e = tx.QueryRowContext(r.Context(), "SELECT active FROM students WHERE id=$1 FOR UPDATE", in.StudentID).Scan(&active); e != nil {
		dbFail(w, e)
		return
	}
	if !active {
		fail(w, 400, "Ученик находится в архиве")
		return
	}
	token, e := Token()
	if e != nil {
		dbFail(w, e)
		return
	}
	_, e = tx.ExecContext(r.Context(), "INSERT INTO telegram_invites(token_hash,student_id,expires_at) VALUES($1,$2,now()+interval '30 minutes') ON CONFLICT(student_id) DO UPDATE SET token_hash=EXCLUDED.token_hash,expires_at=EXCLUDED.expires_at", Hash(token), in.StudentID)
	if e != nil {
		dbFail(w, e)
		return
	}
	if e = tx.Commit(); e != nil {
		dbFail(w, e)
		return
	}
	link := ""
	if api.Config.BotUsername != "" {
		link = "https://t.me/" + api.Config.BotUsername + "?start=" + token
	}
	writeJSON(w, 200, map[string]string{"command": "/start " + token, "url": link, "expires": "30 минут"})
}
func (api *API) unlink(w http.ResponseWriter, r *http.Request, u User) {
	var in struct {
		StudentID int64 `json:"student_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if u.Role == "student" {
		in.StudentID = u.StudentID
	}
	if in.StudentID < 1 {
		fail(w, 400, "Выберите ученика")
		return
	}
	tx, e := api.DB.BeginTx(r.Context(), nil)
	if e != nil {
		dbFail(w, e)
		return
	}
	defer tx.Rollback()
	var id int64
	if e = tx.QueryRowContext(r.Context(), "SELECT id FROM students WHERE id=$1 FOR UPDATE", in.StudentID).Scan(&id); e != nil {
		dbFail(w, e)
		return
	}
	if _, e = tx.ExecContext(r.Context(), "DELETE FROM telegram_links WHERE student_id=$1", id); e != nil {
		dbFail(w, e)
		return
	}
	if _, e = tx.ExecContext(r.Context(), "DELETE FROM telegram_invites WHERE student_id=$1", id); e != nil {
		dbFail(w, e)
		return
	}
	if e = tx.Commit(); e != nil {
		dbFail(w, e)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
