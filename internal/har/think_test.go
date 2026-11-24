package har

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestParseThinkTime(t *testing.T) {
	for in, want := range map[string]ThinkTime{
		"recorded":   {Mode: ThinkRecorded},
		"none":       {Mode: ThinkNone},
		"capped:5s":  {Mode: ThinkCapped, Max: 5 * time.Second},
		"fixed:1.5s": {Mode: ThinkFixed, Max: 1500 * time.Millisecond},
	} {
		got, err := ParseThinkTime(in)
		if err != nil || got != want {
			t.Errorf("ParseThinkTime(%q) = %+v, %v", in, got, err)
		}
		if got.String() != in && in != "fixed:1.5s" {
			t.Errorf("String() = %q, want %q", got.String(), in)
		}
	}
	for _, bad := range []string{"", "sometimes", "capped", "fixed", "capped:soon", "recorded:1s", "none:1s", "fixed:-1s"} {
		if _, err := ParseThinkTime(bad); err == nil {
			t.Errorf("ParseThinkTime(%q) is not an error", bad)
		}
	}
}

// timedHAR has three kept calls, with a style sheet and an analytics call
// left out between the first two: a 2s gap, a 5ms gap and a 7.5s gap.
const timedHAR = `{"log":{"entries":[
 {"startedDateTime":"2026-01-01T10:00:00.000Z","time":100,"request":{"method":"GET","url":"https://app.test/a","headers":[]},"response":{"status":200,"content":{"mimeType":"application/json","text":"{}"}},"_resourceType":"xhr"},
 {"startedDateTime":"2026-01-01T10:00:00.500Z","time":50,"request":{"method":"GET","url":"https://app.test/s.css","headers":[]},"response":{"status":200,"content":{"mimeType":"text/css"}},"_resourceType":"stylesheet"},
 {"startedDateTime":"2026-01-01T10:00:01.000Z","time":50,"request":{"method":"GET","url":"https://www.google-analytics.com/collect","headers":[]},"response":{"status":200,"content":{}},"_resourceType":"xhr"},
 {"startedDateTime":"2026-01-01T10:00:02.100Z","time":100,"request":{"method":"GET","url":"https://app.test/b","headers":[]},"response":{"status":200,"content":{"mimeType":"application/json","text":"{}"}},"_resourceType":"xhr"},
 {"startedDateTime":"2026-01-01T10:00:02.205Z","time":95,"request":{"method":"GET","url":"https://app.test/c","headers":[]},"response":{"status":200,"content":{"mimeType":"application/json","text":"{}"}},"_resourceType":"xhr"},
 {"startedDateTime":"2026-01-01T10:00:09.800Z","time":10,"request":{"method":"GET","url":"https://app.test/d","headers":[]},"response":{"status":200,"content":{"mimeType":"application/json","text":"{}"}},"_resourceType":"xhr"}
]}}`

var sleepRe = regexp.MustCompile(`sleep\(([0-9.]+)\);`)

func sleeps(t *testing.T, mode string) []string {
	t.Helper()
	tt, err := ParseThinkTime(mode)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Convert(strings.NewReader(timedHAR), "t.har", Options{ThinkTime: tt})
	if err != nil {
		t.Fatal(err)
	}
	if res.Requests != 4 || res.Skipped[skipStatic] != 1 || res.Skipped[skipThirdParty] != 1 {
		t.Fatalf("kept %d, skipped %v", res.Requests, res.Skipped)
	}
	var out []string
	for _, m := range sleepRe.FindAllStringSubmatch(res.Script, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestConvert_ThinkTimeModes(t *testing.T) {
	for mode, want := range map[string]string{
		"recorded":  "2,7.5",
		"capped:5s": "2,5",
		"fixed:1s":  "1,1,1",
		"none":      "",
	} {
		if got := strings.Join(sleeps(t, mode), ","); got != want {
			t.Errorf("-think-time %s: sleeps %q, want %q", mode, got, want)
		}
	}
	// The zero Options are the default, recorded.
	res, err := Convert(strings.NewReader(timedHAR), "t.har", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Script, "sleep(2);") || !strings.Contains(res.Script, "-think-time recorded") {
		t.Errorf("the default is not recorded:\n%s", res.Script)
	}
}

func TestConvert_GroupPerPageAndNames(t *testing.T) {
	in := `{"log":{"pages":[{"id":"p1","title":"Home"},{"id":"p2","title":""}],"entries":[
 {"pageref":"p1","startedDateTime":"2026-01-01T10:00:00Z","time":10,"request":{"method":"GET","url":"https://app.test/","headers":[]},"response":{"status":200,"content":{}},"_resourceType":"document"},
 {"pageref":"p1","startedDateTime":"2026-01-01T10:00:00.1Z","time":10,"request":{"method":"GET","url":"https://app.test/users/123/orders/550e8400-e29b-41d4-a716-446655440000?x=1","headers":[]},"response":{"status":200,"content":{}},"_resourceType":"xhr"},
 {"pageref":"p2","startedDateTime":"2026-01-01T10:00:01.11Z","time":10,"request":{"method":"GET","url":"https://app.test/cart","headers":[]},"response":{"status":200,"content":{}},"_resourceType":"xhr"},
 {"pageref":"p3","startedDateTime":"2026-01-01T10:00:01.2Z","time":10,"request":{"method":"GET","url":"https://app.test/pay","headers":[]},"response":{"status":200,"content":{}},"_resourceType":"xhr"}
]}}`
	res, err := Convert(strings.NewReader(in), "p.har", Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  group(\"Home\", () => {\n    // 1. GET /",
		`name: "/users/{id}/orders/{id}",`,
		"  });\n\n  sleep(1);\n  group(\"p2\", () => {",
		`group("p3", () => {`,
		"// 2. GET /users/123/orders/550e8400-e29b-41d4-a716-446655440000 (the recording had 200)",
	} {
		if !strings.Contains(res.Script, want) {
			t.Errorf("missing %q in\n%s", want, res.Script)
		}
	}

	// A recording without pages has no groups.
	res, err = Convert(strings.NewReader(timedHAR), "t.har", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Script, "group(\"") || strings.Contains(res.Script, "group()") {
		t.Errorf("a recording without pages has a group:\n%s", res.Script)
	}
}

func TestConvert_MultipartIsRebuilt(t *testing.T) {
	in := `{"log":{"entries":[
 {"request":{"method":"POST","url":"https://app.test/upload","headers":[{"name":"Content-Type","value":"multipart/form-data; boundary=----WebKitFormBoundaryAbc"}],
  "postData":{"mimeType":"multipart/form-data; boundary=----WebKitFormBoundaryAbc","params":[
   {"name":"title","value":"Hello"},
   {"name":"password","value":"hunter2"},
   {"name":"notes","fileName":"notes.txt","contentType":"text/plain","value":"line one"},
   {"name":"photo","fileName":"cat.png","contentType":"image/png"}
  ]}},"response":{"status":200,"content":{}},"_resourceType":"xhr"},
 {"request":{"method":"POST","url":"https://app.test/upload2","headers":[],
  "postData":{"mimeType":"multipart/form-data; boundary=XyZ","text":"--XyZ\r\nContent-Disposition: form-data; name=\"a\"\r\n\r\n1\r\n--XyZ\r\nContent-Disposition: form-data; name=\"f\"; filename=\"f.txt\"\r\nContent-Type: text/plain\r\n\r\nhi\r\n--XyZ--\r\n"}},
  "response":{"status":200,"content":{}},"_resourceType":"xhr"}
]}}`
	res, err := Convert(strings.NewReader(in), "m.har", Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"Content-Type": "multipart/form-data; boundary=vegaload-form-1",`,
		`body: multipart("vegaload-form-1", [`,
		`{ name: "title", value: "Hello" },`,
		`{ name: "password", value: env.VL_PASSWORD },`,
		`{ name: "notes", fileName: "notes.txt", contentType: "text/plain", value: "line one" },`,
		`{ name: "photo", fileName: "cat.png", contentType: "image/png", value: "REPLACE WITH THE CONTENT OF cat.png" },`,
		`TODO: the file "cat.png" of the form field "photo" is not in the`,
		`"Content-Type": "multipart/form-data; boundary=vegaload-form-2",`,
		`{ name: "a", value: "1" },`,
		`{ name: "f", fileName: "f.txt", contentType: "text/plain", value: "hi" },`,
		`function multipart(boundary, parts) {`,
	} {
		if !strings.Contains(res.Script, want) {
			t.Errorf("missing %q in\n%s", want, res.Script)
		}
	}
	if strings.Contains(res.Script, "hunter2") || strings.Contains(res.Script, "WebKitFormBoundary") {
		t.Errorf("the secret or the recorded boundary stayed:\n%s", res.Script)
	}
}
