package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func signed(q url.Values, token string) string {
	q.Del("hash")
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
	key.Write([]byte(token))
	mac := hmac.New(sha256.New, key.Sum(nil))
	mac.Write([]byte(strings.Join(lines, "\n")))
	q.Set("hash", hex.EncodeToString(mac.Sum(nil)))
	return q.Encode()
}
func TestMiniAppSignature(t *testing.T) {
	now := time.Now()
	q := url.Values{"auth_date": {strconv.FormatInt(now.Unix(), 10)}, "user": {`{"id":1234567890123,"username":"pupil"}`}, "signature": {"new-field-signed-too"}}
	raw := signed(q, "secret")
	u, e := VerifyMiniApp(raw, "secret", now)
	if e != nil || u.ID != 1234567890123 {
		t.Fatal(u, e)
	}
	for _, bad := range []string{raw + "&user=%7B%7D", strings.Replace(raw, "pupil", "admin", 1), "", raw + "&hash=aaa"} {
		if _, e := VerifyMiniApp(bad, "secret", now); e == nil {
			t.Fatal("accepted tampering")
		}
	}
	if _, e := VerifyMiniApp(raw, "wrong", now); e == nil {
		t.Fatal("wrong bot accepted")
	}
	if _, e := VerifyMiniApp(raw, "secret", now.Add(6*time.Minute)); e == nil {
		t.Fatal("expired accepted")
	}
	if _, e := VerifyMiniApp(raw, "secret", now.Add(-time.Minute)); e == nil {
		t.Fatal("future accepted")
	}
}
