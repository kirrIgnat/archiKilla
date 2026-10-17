package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"
)

//go:embed schema.sql
var schema string

type Config struct {
	Addr, DatabaseURL, Origin, AdminLogin, AdminPassword, BotToken, BotUsername string
	Secure, IPv4                                                                bool
	AdminTelegramID                                                             int64
	Location                                                                    *time.Location
}

func Env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func LoadConfig() (Config, error) {
	c := Config{Addr: Env("ADDR", ":8083"), DatabaseURL: os.Getenv("DATABASE_URL"), Origin: strings.TrimRight(Env("PUBLIC_URL", "http://localhost:8083"), "/"), AdminLogin: Env("ADMIN_LOGIN", "artem"), AdminPassword: os.Getenv("ADMIN_PASSWORD"), BotToken: os.Getenv("TELEGRAM_BOT_TOKEN"), BotUsername: strings.TrimPrefix(os.Getenv("TELEGRAM_BOT_USERNAME"), "@"), Secure: Env("COOKIE_SECURE", "true") == "true", IPv4: Env("TELEGRAM_IPV4_ONLY", "false") == "true"}
	var err error
	c.Location, err = time.LoadLocation(Env("APP_TIMEZONE", "Europe/Moscow"))
	if err != nil {
		return c, fmt.Errorf("invalid APP_TIMEZONE")
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	u, err := url.Parse(c.Origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return c, errors.New("PUBLIC_URL must be an origin, for example https://study.example.ru")
	}
	if c.Secure && u.Scheme != "https" {
		return c, errors.New("COOKIE_SECURE=true requires HTTPS PUBLIC_URL")
	}
	if raw := os.Getenv("TELEGRAM_ADMIN_ID"); raw != "" {
		c.AdminTelegramID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || c.AdminTelegramID <= 0 {
			return c, errors.New("invalid TELEGRAM_ADMIN_ID")
		}
	}
	return c, nil
}

type App struct {
	DB     *sql.DB
	Config Config
}

func Open(ctx context.Context, c Config) (*App, error) {
	db, err := sql.Open("pgx", c.DatabaseURL)
	if err != nil {
		return nil, errors.New("database configuration invalid")
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, errors.New("cannot connect to PostgreSQL")
	}
	return &App{DB: db, Config: c}, nil
}
func (a *App) Migrate(ctx context.Context) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(8173401)"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, schema); err != nil {
		return err
	}
	return tx.Commit()
}
func (a *App) Bootstrap(ctx context.Context) error {
	var n int
	if err := a.DB.QueryRowContext(ctx, "SELECT count(*) FROM users WHERE role='admin'").Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if !ValidLogin(a.Config.AdminLogin) {
		return errors.New("invalid ADMIN_LOGIN")
	}
	hash, err := PasswordHash(a.Config.AdminPassword)
	if err != nil {
		return fmt.Errorf("ADMIN_PASSWORD: %w", err)
	}
	_, err = a.DB.ExecContext(ctx, "INSERT INTO users(login,password_hash,role) VALUES($1,$2,'admin') ON CONFLICT(login) DO NOTHING", strings.ToLower(a.Config.AdminLogin), hash)
	return err
}
func PasswordHash(p string) (string, error) {
	if len(p) == 0 {
		return "", errors.New("Введите пароль")
	}
	if len(p) > 72 {
		return "", errors.New("Пароль не должен превышать 72 байта")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
	return string(h), err
}
func ValidLogin(s string) bool {
	if len(s) < 3 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-@", r)) {
			return false
		}
	}
	return true
}
func Token() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func Hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func IsConflict(err error) bool {
	var p *pgconn.PgError
	return errors.As(err, &p) && (p.Code == "23505" || p.Code == "23P01" || p.Code == "23503" || p.Code == "23514")
}
func ValidURL(s string) bool {
	if s == "" {
		return true
	}
	u, e := url.Parse(s)
	return e == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && len(s) <= 2000
}
func Money(n int64) string {
	sign := ""
	if n < 0 {
		sign = "-"
		n = -n
	}
	return fmt.Sprintf("%s%d,%02d руб.", sign, n/100, n%100)
}

type User struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Role      string `json:"role"`
	StudentID int64  `json:"student_id"`
}
type Student struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Phone    string `json:"phone"`
	Subject  string `json:"subject"`
	Notes    string `json:"notes"`
	Active   bool   `json:"active"`
	Login    string `json:"login"`
	Telegram bool   `json:"telegram"`
	Charges  int64  `json:"charges_kop"`
	Paid     int64  `json:"paid_kop"`
}
type Lesson struct {
	ID          int64     `json:"id"`
	StudentID   int64     `json:"student_id"`
	StudentName string    `json:"student_name"`
	Title       string    `json:"title"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at"`
	Location    string    `json:"location"`
	Price       int64     `json:"price_kop"`
	Status      string    `json:"status"`
	Notes       string    `json:"notes"`
}
type Homework struct {
	ID          int64     `json:"id"`
	StudentID   int64     `json:"student_id"`
	StudentName string    `json:"student_name"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	ResourceURL string    `json:"resource_url"`
	DueAt       time.Time `json:"due_at"`
	Status      string    `json:"status"`
	Answer      string    `json:"answer"`
	AnswerURL   string    `json:"answer_url"`
	Feedback    string    `json:"feedback"`
}
type Payment struct {
	ID          int64     `json:"id"`
	StudentID   int64     `json:"student_id"`
	StudentName string    `json:"student_name"`
	Amount      int64     `json:"amount_kop"`
	PaidAt      time.Time `json:"paid_at"`
	Method      string    `json:"method"`
	Note        string    `json:"note"`
	VoidReason  string    `json:"void_reason"`
}

func (a *App) Students(ctx context.Context, id int64) ([]Student, error) {
	rows, e := a.DB.QueryContext(ctx, `SELECT s.id,s.name,s.phone,s.subject,s.notes,s.active,u.login,EXISTS(SELECT 1 FROM telegram_links t WHERE t.student_id=s.id),COALESCE((SELECT sum(price_kop) FROM lessons l WHERE l.student_id=s.id AND status='completed'),0),COALESCE((SELECT sum(amount_kop) FROM payments p WHERE p.student_id=s.id AND void_reason=''),0) FROM students s JOIN users u ON u.student_id=s.id WHERE ($1::bigint=0 OR s.id=$1) ORDER BY s.active DESC,s.name,s.id`, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Student{}
	for rows.Next() {
		var s Student
		if e = rows.Scan(&s.ID, &s.Name, &s.Phone, &s.Subject, &s.Notes, &s.Active, &s.Login, &s.Telegram, &s.Charges, &s.Paid); e != nil {
			return nil, e
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
func (a *App) Lessons(ctx context.Context, id int64, from, to time.Time) ([]Lesson, error) {
	rows, e := a.DB.QueryContext(ctx, `SELECT l.id,l.student_id,s.name,l.title,l.starts_at,l.ends_at,l.location,l.price_kop,l.status,l.notes FROM lessons l JOIN students s ON s.id=l.student_id WHERE ($1::bigint=0 OR l.student_id=$1) AND l.starts_at >= $2 AND l.starts_at < $3 ORDER BY l.starts_at,l.id`, id, from, to)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Lesson{}
	for rows.Next() {
		var l Lesson
		if e = rows.Scan(&l.ID, &l.StudentID, &l.StudentName, &l.Title, &l.StartsAt, &l.EndsAt, &l.Location, &l.Price, &l.Status, &l.Notes); e != nil {
			return nil, e
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
func (a *App) HomeworkList(ctx context.Context, id int64) ([]Homework, error) {
	rows, e := a.DB.QueryContext(ctx, `SELECT h.id,h.student_id,s.name,h.title,h.body,h.resource_url,h.due_at,h.status,h.answer,h.answer_url,h.feedback FROM homework h JOIN students s ON s.id=h.student_id WHERE ($1::bigint=0 OR h.student_id=$1) ORDER BY CASE WHEN h.status='reviewed' THEN 1 ELSE 0 END,h.due_at,h.id`, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Homework{}
	for rows.Next() {
		var h Homework
		if e = rows.Scan(&h.ID, &h.StudentID, &h.StudentName, &h.Title, &h.Body, &h.ResourceURL, &h.DueAt, &h.Status, &h.Answer, &h.AnswerURL, &h.Feedback); e != nil {
			return nil, e
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
func (a *App) Payments(ctx context.Context, id int64) ([]Payment, error) {
	rows, e := a.DB.QueryContext(ctx, `SELECT p.id,p.student_id,s.name,p.amount_kop,p.paid_at,p.method,p.note,p.void_reason FROM payments p JOIN students s ON s.id=p.student_id WHERE ($1::bigint=0 OR p.student_id=$1) ORDER BY p.paid_at DESC,p.id DESC`, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Payment{}
	for rows.Next() {
		var p Payment
		if e = rows.Scan(&p.ID, &p.StudentID, &p.StudentName, &p.Amount, &p.PaidAt, &p.Method, &p.Note, &p.VoidReason); e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
