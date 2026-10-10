package urlerr

import (
	"net/url"
	"strings"
	"testing"
)

func TestInner_DropsTheURL(t *testing.T) {
	_, err := url.Parse("redis://alice:hunter2-secret@[::1")
	if err == nil {
		t.Fatal("want a parse error")
	}
	if !strings.Contains(err.Error(), "hunter2-secret") {
		t.Fatal("test premise: url.Parse should repeat the URL")
	}
	got := Inner(err).Error()
	if strings.Contains(got, "hunter2-secret") || got == "" {
		t.Fatalf("Inner = %q", got)
	}
}

func TestInner_OtherErrorsPassThrough(t *testing.T) {
	e := strings.NewReader("").UnreadByte()
	if Inner(e) != e {
		t.Fatal("a non-URL error must be returned as it is")
	}
}

func TestMask(t *testing.T) {
	cases := map[string]string{
		"redis://alice:hunter2@host:6379":   "redis://alice:***@host:6379",
		"ws://:pw@host":                     "ws://:***@host",
		"mysql://bob@host/db":               "mysql://bob@host/db",
		"host:80":                           "host:80",
		"https://h/path?x=a:b@c":            "https://h/path?x=a:b@c",
		"postgres://u:p%40ss@h/d?sslmode=x": "postgres://u:***@h/d?sslmode=x",
	}
	for in, want := range cases {
		if got := Mask(in); got != want {
			t.Errorf("Mask(%q) = %q, want %q", in, got, want)
		}
	}
}
