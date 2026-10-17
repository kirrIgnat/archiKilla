package app

import "testing"

func TestValidation(t *testing.T) {
	for _, v := range []string{"javascript:alert(1)", "data:text/html,test", "https://user:secret@example.com", "/local"} {
		if ValidURL(v) {
			t.Errorf("unsafe URL accepted: %s", v)
		}
	}
	for _, v := range []string{"", "https://example.com/a?b=c", "http://example.com"} {
		if !ValidURL(v) {
			t.Errorf("valid URL rejected: %s", v)
		}
	}
	if Money(-105) != "-1,05 руб." {
		t.Fatal(Money(-105))
	}
	if ValidLogin("a b") || !ValidLogin("student.one") {
		t.Fatal("login validation")
	}
}
