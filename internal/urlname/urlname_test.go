package urlname

import "testing"

func TestPathAndFold(t *testing.T) {
	cases := []struct{ url, path, folded string }{
		{"http://api.test/users/123", "/users/123", "/users/{id}"},
		{"https://api.test:8443/users/42/orders/7", "/users/42/orders/7", "/users/{id}/orders/{id}"},
		{"http://api.test/items/3F2504E0-4F89-11D3-9A0C-0305E82C3301", "/items/3F2504E0-4F89-11D3-9A0C-0305E82C3301", "/items/{id}"},
		{"http://api.test/items/3f2504e0-4f89-11d3-9a0c-0305e82c3301/x", "/items/3f2504e0-4f89-11d3-9a0c-0305e82c3301/x", "/items/{id}/x"},
		{"http://api.test/blobs/0123456789abcdef", "/blobs/0123456789abcdef", "/blobs/{id}"},
		{"http://api.test/blobs/0123456789ABCDEF0123", "/blobs/0123456789ABCDEF0123", "/blobs/{id}"},
		{"http://api.test/blobs/0123456789abcde", "/blobs/0123456789abcde", "/blobs/0123456789abcde"},
		{"http://api.test/v2/abc", "/v2/abc", "/v2/abc"},
		{"http://api.test/v2/deadbeef", "/v2/deadbeef", "/v2/deadbeef"},
		{"http://api.test/users/1/", "/users/1/", "/users/{id}/"},
		{"http://api.test", "/", "/"},
		{"http://api.test/", "/", "/"},
		{"http://api.test?q=1", "/", "/"},
		{"http://api.test/users/9?page=2#top", "/users/9", "/users/{id}"},
		{"http://api.test/search#frag/1", "/search", "/search"},
		{"http://user:pw@api.test/a/1", "/a/1", "/a/{id}"},
		{"/relative/5", "/relative/5", "/relative/{id}"},
		{"http://api.test/files/12.json", "/files/12.json", "/files/12.json"},
		{"http://api.test/not-a-uuid-3f2504e0-4f89-11d3", "/not-a-uuid-3f2504e0-4f89-11d3", "/not-a-uuid-3f2504e0-4f89-11d3"},
	}
	for _, c := range cases {
		if got := Path(c.url); got != c.path {
			t.Errorf("Path(%q) = %q, want %q", c.url, got, c.path)
		}
		if got := Fold(Path(c.url)); got != c.folded {
			t.Errorf("Fold(Path(%q)) = %q, want %q", c.url, got, c.folded)
		}
	}
}

func TestMethodRowSplit(t *testing.T) {
	for in, want := range map[string]string{"get": "GET", "": "GET", "Post": "POST", "PATCH": "PATCH", "purge": "OTHER", "OPTIONS": "OPTIONS", "head": "HEAD"} {
		if got := Method(in); got != want {
			t.Errorf("Method(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Row("delete", "/users/{id}"); got != "DELETE /users/{id}" {
		t.Errorf("Row = %q", got)
	}
	m, n, ok := Split("GET /users/{id} and more")
	if !ok || m != "GET" || n != "/users/{id} and more" {
		t.Errorf("Split = %q %q %v", m, n, ok)
	}
	for _, bad := range []string{Other, "get /x", "FETCH /x", "GET", "GET ", " /x"} {
		if _, _, ok := Split(bad); ok {
			t.Errorf("Split(%q) ok", bad)
		}
	}
	if m, n, ok := Split("OTHER checkout"); !ok || m != "OTHER" || n != "checkout" {
		t.Errorf("Split OTHER = %q %q %v", m, n, ok)
	}
}

func TestTemplate(t *testing.T) {
	cases := []struct {
		parts, values []string
		url, name     string
	}{
		{[]string{"https://api.test/users/", ""}, []string{"7"}, "https://api.test/users/7", "/users/${}"},
		{[]string{"https://api.test/users/", "/orders/", "?full=", ""}, []string{"7", "9", "1"}, "https://api.test/users/7/orders/9?full=1", "/users/${}/orders/${}"},
		{[]string{"", "/users/", ""}, []string{"http://api.test", "7"}, "http://api.test/users/7", "/users/${}"},
		{[]string{"", "/users/", ""}, []string{"http://api.test/v1", "7"}, "http://api.test/v1/users/7", "${}/users/${}"},
		{[]string{"https://api.test"}, nil, "https://api.test", "/"},
		{[]string{"https://api.test/health"}, nil, "https://api.test/health", "/health"},
		{[]string{"https://", "/x"}, []string{"api.test"}, "https://api.test/x", "/x"},
		{[]string{"https://api.test/users/", ""}, []string{""}, "https://api.test/users/", "/users/${}"},
		{[]string{"https://api.test/a#", ""}, []string{"frag"}, "https://api.test/a#frag", "/a"},
	}
	for _, c := range cases {
		u, n := Template(c.parts, c.values)
		if u != c.url || n != c.name {
			t.Errorf("Template(%q, %q) = %q, %q; want %q, %q", c.parts, c.values, u, n, c.url, c.name)
		}
	}
}
