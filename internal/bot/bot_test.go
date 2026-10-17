package bot

import (
	"artem-study/internal/app"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUnicodeChunks(t *testing.T) {
	s := strings.Repeat("Запись упражнения на гитаре.\n", 1000)
	chunks := splitText(s)
	if strings.Join(chunks, "") != s {
		t.Fatal("text lost")
	}
	for _, p := range chunks {
		if len([]rune(p)) > 3500 {
			t.Fatal("message too long")
		}
	}
}
func TestAPIErrorsAndRedaction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"ok":false,"error_code":429,"description":"sensitive","parameters":{"retry_after":5}}`)
	}))
	defer srv.Close()
	b := &Bot{Client: srv.Client(), BaseURL: srv.URL + "/"}
	e := b.call(context.Background(), "sendMessage", map[string]string{}, nil)
	a, ok := e.(*APIError)
	if !ok || a.Code != 429 || a.RetryAfter != 5 {
		t.Fatal(e)
	}
	if strings.Contains(e.Error(), "sensitive") {
		t.Fatal("leaked API response")
	}
	b.BaseURL = "http://127.0.0.1:1/botSECRET/"
	b.Client = &http.Client{Timeout: time.Second}
	e = b.call(context.Background(), "getUpdates", map[string]string{}, nil)
	if e == nil || strings.Contains(e.Error(), "SECRET") {
		t.Fatal("leaked token in connection error")
	}
}
func TestIgnoreGroupAndMissingSender(t *testing.T) {
	b := &Bot{App: &app.App{}}
	m := &Message{}
	m.Chat.Type = "group"
	if e := b.Handle(context.Background(), m); e != nil {
		t.Fatal(e)
	}
}
