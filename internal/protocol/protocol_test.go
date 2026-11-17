package protocol

import (
	"strings"
	"testing"
	"time"
)

func TestHTTPStatusOK(t *testing.T) {
	for code, want := range map[int]bool{
		101: false, 199: false,
		200: true, 201: true, 204: true, 299: true,
		301: true, 302: true, 304: true, 399: true,
		400: false, 401: false, 404: false, 429: false, 499: false,
		500: false, 502: false, 503: false, 599: false,
	} {
		if got := HTTPStatusOK(code); got != want {
			t.Errorf("HTTPStatusOK(%d) = %v, want %v", code, got, want)
		}
	}
}

func TestTargetOption(t *testing.T) {
	tg := Target{Options: map[string]string{"read": "64", "wait": "250ms", "mode": "publish"}}

	if got := tg.Option("mode", "x"); got != "publish" {
		t.Errorf("Option(mode) = %q", got)
	}
	if got := tg.Option("missing", "x"); got != "x" {
		t.Errorf("Option(missing) = %q, want the default", got)
	}
	if n, err := tg.OptionInt("read", 0); err != nil || n != 64 {
		t.Errorf("OptionInt(read) = %d, %v", n, err)
	}
	if n, err := tg.OptionInt("missing", 7); err != nil || n != 7 {
		t.Errorf("OptionInt(missing) = %d, %v", n, err)
	}
	if d, err := tg.OptionDuration("wait", 0); err != nil || d != 250*time.Millisecond {
		t.Errorf("OptionDuration(wait) = %v, %v", d, err)
	}
	if d, err := tg.OptionDuration("missing", time.Second); err != nil || d != time.Second {
		t.Errorf("OptionDuration(missing) = %v, %v", d, err)
	}
}

func TestTargetOption_BadValuesAreErrors(t *testing.T) {
	tg := Target{Options: map[string]string{"read": "lots", "wait": "soon"}}
	if _, err := tg.OptionInt("read", 0); err == nil {
		t.Error("OptionInt on text: want an error")
	}
	if _, err := tg.OptionDuration("wait", 0); err == nil {
		t.Error("OptionDuration on text: want an error")
	}
}

func TestTargetOption_NilMap(t *testing.T) {
	var tg Target
	if got := tg.Option("a", "d"); got != "d" {
		t.Errorf("nil Options: got %q", got)
	}
	if n, err := tg.OptionInt("a", 3); err != nil || n != 3 {
		t.Errorf("nil Options: got %d, %v", n, err)
	}
}

func TestRejectUnknownOptions(t *testing.T) {
	tg := Target{Options: map[string]string{"read": "1", "raed": "2"}}
	err := tg.RejectUnknownOptions("until", "read")
	if err == nil {
		t.Fatal("want an error for raed")
	}
	for _, want := range []string{"raed", "read, until"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
	if err := (Target{}).RejectUnknownOptions("read"); err != nil {
		t.Errorf("no options: %v", err)
	}
	if err := (Target{}).RejectUnknownOptions(); err != nil {
		t.Errorf("no options and none allowed: %v", err)
	}
	if err := (Target{Options: map[string]string{"read": "1"}}).RejectUnknownOptions("read"); err != nil {
		t.Errorf("known option: %v", err)
	}
}

func TestRejectUnknownOptions_NoneAllowed(t *testing.T) {
	err := (Target{Options: map[string]string{"read": "1"}}).RejectUnknownOptions()
	if err == nil || !strings.Contains(err.Error(), "takes no options") {
		t.Fatalf("err = %v, want \"takes no options\"", err)
	}
}

func TestOptionBool(t *testing.T) {
	tg := Target{Options: map[string]string{"a": "true", "b": "false", "c": "maybe"}}
	if v, err := tg.OptionBool("a", false); err != nil || !v {
		t.Errorf("a = %v, %v", v, err)
	}
	if v, err := tg.OptionBool("b", true); err != nil || v {
		t.Errorf("b = %v, %v", v, err)
	}
	if v, err := tg.OptionBool("missing", true); err != nil || !v {
		t.Errorf("missing = %v, %v", v, err)
	}
	if _, err := tg.OptionBool("c", false); err == nil {
		t.Error("c: want an error")
	}
}
