package har

import (
	"os"
	"strings"
	"testing"
)

func convertShop(t *testing.T, host string, opt Options) *Result {
	t.Helper()
	data, err := os.ReadFile("testdata/shop.har")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(data), "HOST", host)
	res, err := Convert(strings.NewReader(text), "shop.har", opt)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestConvert_KeepsTheApiCallsInOrder(t *testing.T) {
	res := convertShop(t, "shop.example.com", Options{})
	if res.Total != 12 {
		t.Errorf("Total = %d, want 12", res.Total)
	}
	// document, login, orders get, orders post, delete, old, upload
	if res.Requests != 7 {
		t.Fatalf("Requests = %d, want 7\n%s", res.Requests, res.Script)
	}
	var order []string
	for _, want := range []string{"http.get(\"https://shop.example.com/\"", "/api/login", "/api/orders?user=", "http.post(\"https://shop.example.com/api/orders\"", "http.delete(", "/old", "/api/upload"} {
		i := strings.Index(res.Script, want)
		if i < 0 {
			t.Fatalf("missing %q in\n%s", want, res.Script)
		}
		order = append(order, want)
		_ = i
	}
	prev := -1
	for _, want := range order {
		i := strings.Index(res.Script, want)
		if i < prev {
			t.Errorf("%q is out of order", want)
		}
		prev = i
	}
}

func TestConvert_SkipsAndSaysWhy(t *testing.T) {
	res := convertShop(t, "shop.example.com", Options{})
	for why, n := range map[string]int{skipStatic: 2, skipThirdParty: 1, skipPreflight: 1, skipFailed: 1} {
		if res.Skipped[why] != n {
			t.Errorf("Skipped[%q] = %d, want %d", why, res.Skipped[why], n)
		}
	}
	if len(res.Hosts) != 1 || res.Hosts[0] != "shop.example.com" {
		t.Errorf("Hosts = %v", res.Hosts)
	}
}

func TestConvert_IncludeStaticAndThirdParty(t *testing.T) {
	res := convertShop(t, "shop.example.com", Options{IncludeStatic: true, IncludeThirdParty: true})
	if res.Requests != 10 {
		t.Errorf("Requests = %d, want 10", res.Requests)
	}
	if !strings.Contains(res.Script, "tracker.other-site.net") || !strings.Contains(res.Script, "logo.png") {
		t.Error("the static file and the other site should be kept")
	}
}

func TestConvert_HostOption(t *testing.T) {
	res := convertShop(t, "shop.example.com", Options{Hosts: []string{"tracker.other-site.net"}, IncludeStatic: true})
	if res.Requests != 1 || res.Hosts[0] != "tracker.other-site.net" {
		t.Errorf("Requests = %d, Hosts = %v", res.Requests, res.Hosts)
	}
}

func TestConvert_MaxRequests(t *testing.T) {
	res := convertShop(t, "shop.example.com", Options{MaxRequests: 2})
	if res.Requests != 2 || res.Skipped[skipLimit] == 0 {
		t.Errorf("Requests = %d, Skipped = %v", res.Requests, res.Skipped)
	}
}

func TestConvert_NoSecretReachesTheFile(t *testing.T) {
	res := convertShop(t, "shop.example.com", Options{})
	for _, secret := range []string{"hunter2", "sid=abc", "KEY123", "eyJhbGciOiJIUzI1NiJ9", "zzz"} {
		if strings.Contains(res.Script, secret) {
			t.Errorf("the secret %q is in the scenario:\n%s", secret, res.Script)
		}
	}
	want := []string{"VL_API_KEY", "VL_AUTHORIZATION", "VL_COOKIE", "VL_CSRF_TOKEN", "VL_PASSWORD"}
	if strings.Join(res.EnvNames, ",") != strings.Join(want, ",") {
		t.Errorf("EnvNames = %v, want %v", res.EnvNames, want)
	}
	for _, line := range []string{
		`"Authorization": env.VL_AUTHORIZATION`,
		`"Cookie": env.VL_COOKIE`,
		`"password": env.VL_PASSWORD`,
		`&api_key=" + encodeURIComponent(env.VL_API_KEY)`,
		`"sku=A1&qty=2&csrf_token=" + encodeURIComponent(env.VL_CSRF_TOKEN)`,
		`-secret-env VL_API_KEY`,
	} {
		if !strings.Contains(res.Script, line) {
			t.Errorf("missing %q in\n%s", line, res.Script)
		}
	}
}

func TestConvert_DropsHeadersTheClientSetsItself(t *testing.T) {
	res := convertShop(t, "shop.example.com", Options{})
	for _, h := range []string{"User-Agent", "Accept-Language", "Sec-Fetch-Mode", "Origin", "Referer", "Content-Length", `"Host"`} {
		if strings.Contains(res.Script, `"`+strings.Trim(h, `"`)+`":`) {
			t.Errorf("header %s should have been dropped", h)
		}
	}
	if !strings.Contains(res.Script, `"Content-Type": "application/json"`) || !strings.Contains(res.Script, `"Accept": "application/json"`) {
		t.Error("Content-Type and Accept should be kept")
	}
}

func TestConvert_JSONBodyKeepsOrderAndTypes(t *testing.T) {
	res := convertShop(t, "shop.example.com", Options{})
	want := "body: JSON.stringify({\n" +
		"      \"email\": \"a@example.com\",\n" +
		"      \"password\": env.VL_PASSWORD,\n" +
		"      \"remember\": true,\n" +
		"      \"plan\": null,\n" +
		"      \"tags\": [\n" +
		"        \"a\",\n" +
		"        \"b\",\n" +
		"      ],\n" +
		"    }),"
	if !strings.Contains(res.Script, want) {
		t.Errorf("JSON body not as expected in\n%s", res.Script)
	}
}

func TestConvert_FlagsDynamicValuesAndFindsTheirSource(t *testing.T) {
	res := convertShop(t, "shop.example.com", Options{})
	// The user id in the orders request came from the login answer.
	flat := strings.Join(strings.Fields(strings.ReplaceAll(res.Script, "//", " ")), " ")
	if !strings.Contains(flat, "It was in the answer to request 2 (POST /api/login)") {
		t.Errorf("the source of the user id was not found in\n%s", res.Script)
	}
	if !strings.Contains(res.Script, "looks like a UUID") {
		t.Error("a UUID should be flagged")
	}
	// The request id header has no earlier source: flagged, no source.
	if res.Todos < 3 {
		t.Errorf("Todos = %d, want at least 3", res.Todos)
	}
}

func TestConvert_ExpectedStatus(t *testing.T) {
	res := convertShop(t, "shop.example.com", Options{})
	for _, want := range []string{
		`expectStatus(r2, 200, "POST /api/login");`,
		`expectStatus(r4, 201, "POST /api/orders");`,
		`expectStatus(r5, 204, "DELETE /api/orders/42");`,
		`expectStatus(r6, 0, "GET /old");`, // a redirect: any answer below 400
	} {
		if !strings.Contains(res.Script, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestConvert_MultipartBodyIsLeftOutWithANote(t *testing.T) {
	res := convertShop(t, "shop.example.com", Options{})
	if !strings.Contains(res.Script, "multipart form") || strings.Contains(res.Script, "--x") {
		t.Errorf("the multipart body should be left out with a note:\n%s", res.Script)
	}
}

func TestConvert_PageTitleIsAComment(t *testing.T) {
	res := convertShop(t, "shop.example.com", Options{})
	if !strings.Contains(res.Script, "// ---- page: Shop home") {
		t.Error("the page title should appear once as a comment")
	}
}

func TestConvert_AddressHostsAreTheirOwnSite(t *testing.T) {
	res := convertShop(t, "127.0.0.1:8080", Options{})
	if res.Requests != 7 {
		t.Errorf("Requests = %d, want 7 (an address must not be cut to its last two numbers)", res.Requests)
	}
}

func TestConvert_NotAHAR(t *testing.T) {
	for _, in := range []string{"", "nope", `{}`, `{"log":{}}`, `[1]`} {
		if _, err := Convert(strings.NewReader(in), "x.har", Options{}); err == nil {
			t.Errorf("%q should be refused", in)
		}
	}
}

func TestSite(t *testing.T) {
	for host, want := range map[string]string{
		"shop.example.com": "example.com", "api.shop.example.com:8443": "example.com",
		"example.co.uk": "example.co.uk", "www.example.co.uk": "example.co.uk",
		"localhost:3000": "localhost", "10.1.2.3:80": "10.1.2.3", "[::1]:80": "[::1]",
	} {
		if got := site(host); got != want {
			t.Errorf("site(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestIsSecretName(t *testing.T) {
	for n, want := range map[string]bool{
		"password": true, "Password": true, "api_key": true, "X-API-Key": true,
		"access_token": true, "csrfToken": true, "sessionId": true, "Authorization": true,
		"author": false, "email": false, "limit": false, "name": false, "sort_key": false,
	} {
		if got := isSecretName(n); got != want {
			t.Errorf("isSecretName(%q) = %v, want %v", n, got, want)
		}
	}
}

func TestDynamicKind(t *testing.T) {
	for v, want := range map[string]bool{
		"550e8400-e29b-41d4-a716-446655440000": true, "eyJhbGciOiJIUzI1NiJ9.e30.abc": true,
		"deadbeefdeadbeefdeadbeef": true, "1696600000000": true, "Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5": true,
		"42": false, "hello": false, "a-normal-slug": false, "2026": false,
	} {
		if got := dynamicKind(v) != ""; got != want {
			t.Errorf("dynamicKind(%q) = %v, want %v", v, got, want)
		}
	}
}

// harOf builds a small recording from entries given as method, url,
// resource type, status and an optional form or json body.
func harOf(t *testing.T, entries ...[5]string) string {
	t.Helper()
	var parts []string
	for _, e := range entries {
		post := ""
		if e[4] != "" {
			post = `,"postData":{"mimeType":"application/json","text":` + jsString(e[4]) + `}`
		}
		parts = append(parts, `{"_resourceType":"`+e[2]+`","request":{"method":"`+e[0]+`","url":"`+e[1]+`","headers":[]`+post+`},"response":{"status":`+e[3]+`,"content":{"mimeType":"application/json"}}}`)
	}
	return `{"log":{"entries":[` + strings.Join(parts, ",") + `]}}`
}

func TestConvert_MainSiteIsTheFirstPageNotTheBusiestHost(t *testing.T) {
	// Analytics are called more often than the app. The app must stay.
	in := harOf(t,
		[5]string{"GET", "https://app.example.com/", "document", "200", ""},
		[5]string{"POST", "https://metrics.other.net/a", "xhr", "200", ""},
		[5]string{"POST", "https://metrics.other.net/b", "xhr", "200", ""},
		[5]string{"POST", "https://metrics.other.net/c", "xhr", "200", ""},
		[5]string{"GET", "https://api.example.com/items", "xhr", "200", ""},
	)
	res, err := Convert(strings.NewReader(in), "x.har", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Requests != 2 || strings.Join(res.Hosts, ",") != "api.example.com,app.example.com" {
		t.Errorf("Requests = %d, Hosts = %v", res.Requests, res.Hosts)
	}
}

func TestConvert_FormParamValueWithAmpersandStaysOneField(t *testing.T) {
	in := `{"log":{"entries":[{"request":{"method":"POST","url":"https://a.example.com/f","headers":[],` +
		`"postData":{"mimeType":"application/x-www-form-urlencoded","text":"","params":[{"name":"q","value":"a&b=c d"},{"name":"n","value":"1"}]}},` +
		`"response":{"status":200,"content":{"mimeType":"text/plain"}}}]}}`
	res, err := Convert(strings.NewReader(in), "x.har", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Script, `body: "q=a%26b%3Dc+d&n=1"`) {
		t.Errorf("the value was not escaped:\n%s", res.Script)
	}
}

func TestConvert_AJWTIsASecretWhateverItIsCalled(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl"
	in := harOf(t,
		[5]string{"GET", "https://a.example.com/x?code=" + jwt, "xhr", "200", ""},
		[5]string{"POST", "https://a.example.com/y", "xhr", "200", `{"data":"` + jwt + `","keep":"hello"}`},
	)
	res, err := Convert(strings.NewReader(in), "x.har", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Script, "eyJhbGci") {
		t.Errorf("a JWT is in the file:\n%s", res.Script)
	}
	for _, want := range []string{"encodeURIComponent(env.VL_CODE)", `"data": env.VL_DATA`, `"keep": "hello"`} {
		if !strings.Contains(res.Script, want) {
			t.Errorf("missing %q in\n%s", want, res.Script)
		}
	}
}

func TestConvert_NamesThatDoNotLookSecretStayAsRecorded(t *testing.T) {
	// This is the limit, and the package comment says so.
	in := harOf(t, [5]string{"GET", "https://a.example.com/reset/abc?code=xyz", "xhr", "200", ""})
	res, err := Convert(strings.NewReader(in), "x.har", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Script, "/reset/abc?code=xyz") {
		t.Errorf("an ordinary name should stay as recorded:\n%s", res.Script)
	}
}
