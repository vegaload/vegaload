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
	for _, secret := range []string{"hunter2", "sid=abc", "KEY123", "eyJhbGciOiJIUzI1NiJ9", "550e8400-e29b-41d4-a716-446655440000"} {
		if strings.Contains(res.Script, secret) {
			t.Errorf("the secret %q is in the scenario:\n%s", secret, res.Script)
		}
	}
	want := []string{"VL_API_KEY", "VL_COOKIE", "VL_PASSWORD"}
	if strings.Join(res.EnvNames, ",") != strings.Join(want, ",") {
		t.Errorf("EnvNames = %v, want %v", res.EnvNames, want)
	}
	for _, line := range []string{
		`"Authorization": "Bearer " + c2_token`,
		`const c2_token = r2.json().token`,
		`const c2_userId = r2.json().userId`,
		`encodeURIComponent(c2_userId)`,
		`"Cookie": env.VL_COOKIE`,
		`"password": env.VL_PASSWORD`,
		`&api_key=" + encodeURIComponent(env.VL_API_KEY)`,
		`csrf_token=" + "zzz"`,
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
	if strings.Contains(res.Script, "It was in the answer") {
		t.Errorf("a carried value should not stay a TODO:\n%s", res.Script)
	}
	if !strings.Contains(res.Script, "looks like a UUID") {
		t.Error("a UUID with no source should be flagged")
	}
	if !strings.Contains(res.Script, "csrf_token was not in an earlier answer") {
		t.Error("a csrf token with no source should stay a TODO")
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

// A status the recording had at 400 or above is what the call expects, so
// the generated call lists it in expectedStatuses; a lower one does not.
func TestConvert_RecordedErrorStatusIsExpected(t *testing.T) {
	in := harOf(t,
		[5]string{"GET", "https://app.example.com/", "document", "200", ""},
		[5]string{"GET", "https://app.example.com/api/items/9", "xhr", "404", ""},
		[5]string{"GET", "https://app.example.com/api/flaky", "xhr", "503", ""},
	)
	res, err := Convert(strings.NewReader(in), "x.har", Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"const r2 = http.get(\"https://app.example.com/api/items/9\", {\n    expectedStatuses: [404],\n  });",
		"const r3 = http.get(\"https://app.example.com/api/flaky\", {\n    expectedStatuses: [503],\n  });",
	} {
		if !strings.Contains(res.Script, want) {
			t.Errorf("missing %q in:\n%s", want, res.Script)
		}
	}
	if strings.Count(res.Script, "expectedStatuses") != 2 {
		t.Errorf("expectedStatuses should be on the 404 and 503 calls only:\n%s", res.Script)
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

func TestConvert_HTMLInputByIDAndSetCookieAndHeader(t *testing.T) {
	view := "recordedviewstatevalue0001"
	sid := "recorded-session-value-0001"
	rid := "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	in := `{"log":{"entries":[
		{"_resourceType":"document","request":{"method":"GET","url":"https://app.example.com/form","headers":[]},
		 "response":{"status":200,"headers":[
		   {"name":"x-request-id","value":"` + rid + `"},
		   {"name":"set-cookie","value":"sid=` + sid + `; Path=/"},
		   {"name":"set-cookie","value":"theme=dark; Expires=Wed, 21 Oct 2015 07:28:00 GMT"}
		 ],"content":{"mimeType":"text/html","text":"<form><input id=\"viewstate\" type=\"hidden\" value=\"` + view + `\"></form>"}}},
		{"_resourceType":"xhr","request":{"method":"POST","url":"https://app.example.com/save","headers":[
		   {"name":"Cookie","value":"sid=` + sid + `; theme=dark"},
		   {"name":"X-Request-Id","value":"` + rid + `"}
		 ],"postData":{"mimeType":"application/x-www-form-urlencoded","text":"viewstate=` + view + `"}},
		 "response":{"status":200,"content":{"mimeType":"text/plain","text":"ok"}}}
	]}}`
	res, err := Convert(strings.NewReader(in), "x.har", Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`const c1_viewstate = hidden(r1.body, "viewstate")`,
		`const c1_sid = cookie(header(r1, "set-cookie"), "sid")`,
		`const c1_x_request_id = header(r1, "x-request-id")`,
		`function header(`,
		`function cookie(`,
		`function hidden(`,
		`"sid=" + c1_sid + "; " + env.VL_COOKIE`,
		`viewstate=" + encodeURIComponent(c1_viewstate)`,
	} {
		if !strings.Contains(res.Script, want) {
			t.Errorf("missing %q in\n%s", want, res.Script)
		}
	}
	if strings.Contains(res.Script, view) || strings.Contains(res.Script, sid) || strings.Contains(res.Script, rid) {
		t.Errorf("a carried value is still in the file:\n%s", res.Script)
	}
}

func TestConvert_SameHostOnly(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl"
	in := `{"log":{"entries":[
		{"_resourceType":"document","request":{"method":"GET","url":"https://app.example.com/","headers":[]},
		 "response":{"status":200,"content":{"mimeType":"text/html","text":"<html></html>"}}},
		{"_resourceType":"xhr","request":{"method":"POST","url":"https://app.example.com/login","headers":[],
		 "postData":{"mimeType":"application/json","text":"{}"}},
		 "response":{"status":200,"content":{"mimeType":"application/json","text":"{\"token\":\"` + jwt + `\"}"}}},
		{"_resourceType":"xhr","request":{"method":"GET","url":"https://api.example.com/items","headers":[
		   {"name":"Authorization","value":"Bearer ` + jwt + `"}
		 ]},"response":{"status":200,"content":{"mimeType":"application/json","text":"[]"}}}
	]}}`
	res, err := Convert(strings.NewReader(in), "x.har", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Script, "c2_token") || strings.Contains(res.Script, jwt) {
		t.Errorf("a token from another host was carried:\n%s", res.Script)
	}
	if !strings.Contains(res.Script, "env.VL_AUTHORIZATION") {
		t.Errorf("Authorization should stay a secret:\n%s", res.Script)
	}
}

func TestConvert_TwoJSONFieldsAreNotCarried(t *testing.T) {
	id := "550e8400-e29b-41d4-a716-446655440000"
	in := `{"log":{"entries":[
		{"_resourceType":"xhr","request":{"method":"POST","url":"https://app.example.com/login","headers":[],
		 "postData":{"mimeType":"application/json","text":"{}"}},
		 "response":{"status":200,"content":{"mimeType":"application/json","text":"{\"userId\":\"` + id + `\",\"orderId\":\"` + id + `\"}"}}},
		{"_resourceType":"xhr","request":{"method":"GET","url":"https://app.example.com/orders?user=` + id + `","headers":[]},
		 "response":{"status":200,"content":{"mimeType":"application/json","text":"[]"}}}
	]}}`
	res, err := Convert(strings.NewReader(in), "x.har", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Script, "const c1_") {
		t.Errorf("an ambiguous value was carried:\n%s", res.Script)
	}
	if !strings.Contains(res.Script, id) || !strings.Contains(res.Script, "looks like a UUID") {
		t.Errorf("the value should stay, with a TODO:\n%s", res.Script)
	}
}

func TestConvert_NearestResponseWins(t *testing.T) {
	first := "550e8400-e29b-41d4-a716-446655440000"
	second := "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	in := `{"log":{"entries":[
		{"_resourceType":"xhr","request":{"method":"POST","url":"https://app.example.com/login","headers":[],
		 "postData":{"mimeType":"application/json","text":"{}"}},
		 "response":{"status":200,"content":{"mimeType":"application/json","text":"{\"token\":\"` + first + `\"}"}}},
		{"_resourceType":"xhr","request":{"method":"GET","url":"https://app.example.com/a?token=` + first + `","headers":[]},
		 "response":{"status":200,"content":{"mimeType":"application/json","text":"{\"token\":\"` + second + `\"}"}}},
		{"_resourceType":"xhr","request":{"method":"GET","url":"https://app.example.com/b?token=` + second + `","headers":[]},
		 "response":{"status":200,"content":{"mimeType":"application/json","text":"{}"}}}
	]}}`
	res, err := Convert(strings.NewReader(in), "x.har", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Script, "const c1_token = r1.json().token") || !strings.Contains(res.Script, "const c2_token = r2.json().token") {
		t.Fatalf("missing consts in\n%s", res.Script)
	}
	b := res.Script[strings.Index(res.Script, "/b?"):]
	if !strings.Contains(b, "encodeURIComponent(c2_token)") || strings.Contains(b, "c1_token") {
		t.Errorf("the later call should use the refreshed token:\n%s", res.Script)
	}
}

func TestConvert_JSONBeatsSetCookieInTheSameResponse(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl"
	in := `{"log":{"entries":[
		{"_resourceType":"xhr","request":{"method":"POST","url":"https://app.example.com/login","headers":[],
		 "postData":{"mimeType":"application/json","text":"{}"}},
		 "response":{"status":200,"headers":[
		   {"name":"set-cookie","value":"token=` + jwt + `; Path=/"}
		 ],"content":{"mimeType":"application/json","text":"{\"token\":\"` + jwt + `\"}"}}},
		{"_resourceType":"xhr","request":{"method":"GET","url":"https://app.example.com/orders","headers":[
		   {"name":"Authorization","value":"Bearer ` + jwt + `"}
		 ]},"response":{"status":200,"content":{"mimeType":"application/json","text":"[]"}}}
	]}}`
	res, err := Convert(strings.NewReader(in), "x.har", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Script, `const c1_token = r1.json().token`) {
		t.Fatalf("the JSON field should win over Set-Cookie:\n%s", res.Script)
	}
	if !strings.Contains(res.Script, `"Authorization": "Bearer " + c1_token`) {
		t.Errorf("Authorization should use the carried token:\n%s", res.Script)
	}
	if strings.Contains(res.Script, "env.VL_AUTHORIZATION") || strings.Contains(res.Script, "cookie(") || strings.Contains(res.Script, jwt) {
		t.Errorf("the token should not stay a secret or come from the cookie:\n%s", res.Script)
	}
}

func TestConvert_TieDoesNotUseAnOlderResponse(t *testing.T) {
	id := "550e8400-e29b-41d4-a716-446655440000"
	in := `{"log":{"entries":[
		{"_resourceType":"xhr","request":{"method":"GET","url":"https://app.example.com/a","headers":[]},
		 "response":{"status":200,"content":{"mimeType":"application/json","text":"{\"id\":\"` + id + `\"}"}}},
		{"_resourceType":"xhr","request":{"method":"GET","url":"https://app.example.com/b","headers":[]},
		 "response":{"status":200,"content":{"mimeType":"application/json","text":"{\"userId\":\"` + id + `\",\"orderId\":\"` + id + `\"}"}}},
		{"_resourceType":"xhr","request":{"method":"GET","url":"https://app.example.com/orders?user=` + id + `","headers":[]},
		 "response":{"status":200,"content":{"mimeType":"application/json","text":"[]"}}}
	]}}`
	res, err := Convert(strings.NewReader(in), "x.har", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Script, "const c1_") || strings.Contains(res.Script, "const c2_") {
		t.Errorf("a tie must not fall through to an older response:\n%s", res.Script)
	}
	if !strings.Contains(res.Script, id) || !strings.Contains(res.Script, "looks like a UUID") {
		t.Errorf("the value should stay, with a TODO:\n%s", res.Script)
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
