// Package urlname names the report row of an HTTP call: its method, then a
// name such as "/users/{id}". It only reads text, so the scripting runtimes,
// the -protocol runner, the report and an importer can all share one rule.
package urlname

import (
	"net/http"
	"strings"
)

// ID is what Fold puts in place of a path segment that looks like an id.
const ID = "{id}"

// Placeholder stands for one interpolated value in a Template name.
const Placeholder = "${}"

// Other is the row that holds the calls past the row cap.
const Other = "Other"

// Method is the method part of a row: one of the usual HTTP verbs in upper
// case, GET when empty, and OTHER for anything else, so a script cannot
// store an arbitrary string as a method.
func Method(method string) string {
	switch m := strings.ToUpper(method); m {
	case "":
		return http.MethodGet
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return m
	default:
		return "OTHER"
	}
}

// Row is the row name of a call: Method(method), a space, then name.
func Row(method, name string) string {
	return Method(method) + " " + name
}

// Split reads a row name back into its method and name. ok is false when
// row does not start with a method Row writes, such as Other.
func Split(row string) (method, name string, ok bool) {
	method, name, found := strings.Cut(row, " ")
	if !found || name == "" || Method(method) != method || method == "" {
		return "", "", false
	}
	return method, name, true
}

// Path is the path of rawURL as written, without the scheme, host, query
// or fragment. An empty path is "/".
func Path(rawURL string) string {
	start, end := pathSpan(rawURL)
	if p := rawURL[start:end]; p != "" {
		return p
	}
	return "/"
}

// pathSpan is where the path of rawURL starts and ends.
func pathSpan(rawURL string) (start, end int) {
	if i := strings.Index(rawURL, "://"); i >= 0 && !strings.ContainsAny(rawURL[:i], "/?#") {
		start = i + 3
		if j := strings.IndexAny(rawURL[start:], "/?#"); j >= 0 {
			start += j
		} else {
			start = len(rawURL)
		}
	}
	end = len(rawURL)
	if j := strings.IndexAny(rawURL[start:], "?#"); j >= 0 {
		end = start + j
	}
	return start, end
}

// Fold replaces each segment of path that looks like an id with ID: all
// digits, a UUID (8-4-4-4-12 hex digits), or 16 or more hex digits. So
// /users/123 and /users/456 share the row /users/{id}, and /v2/abc stays.
func Fold(path string) string {
	segs := strings.Split(path, "/")
	changed := false
	for i, s := range segs {
		if isID(s) {
			segs[i] = ID
			changed = true
		}
	}
	if !changed {
		return path
	}
	return strings.Join(segs, "/")
}

func isID(s string) bool {
	if s == "" {
		return false
	}
	if allOf(s, isDigit) {
		return true
	}
	if len(s) >= 16 && allOf(s, isHex) {
		return true
	}
	return isUUID(s)
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			if !isHex(s[i]) {
				return false
			}
		}
	}
	return true
}

func allOf(s string, ok func(byte) bool) bool {
	for i := 0; i < len(s); i++ {
		if !ok(s[i]) {
			return false
		}
	}
	return true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHex(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// Template builds a URL from the literal parts of a template and the
// values between them (len(values) == len(parts)-1), as a JavaScript tagged
// template or Python's http.url gives them. It returns the URL and its row
// name: the path part of the template, with each value that falls inside
// the path shown as Placeholder. So "https://api.test/users/" + 7 is named
// "/users/${}". A template with no path is named "/".
func Template(parts, values []string) (rawURL, name string) {
	var u strings.Builder
	type span struct{ start, end int }
	valueAt := make([]span, len(values))
	for i, p := range parts {
		u.WriteString(p)
		if i < len(values) {
			valueAt[i].start = u.Len()
			u.WriteString(values[i])
			valueAt[i].end = u.Len()
		}
	}
	rawURL = u.String()
	start, end := pathSpan(rawURL)

	var n strings.Builder
	pos := 0
	for i, p := range parts {
		writeInside(&n, p, pos, start, end)
		pos += len(p)
		if i < len(values) {
			v := valueAt[i]
			// An empty value counts when it sits in the path or at either
			// end of it, so its row matches the rows of other values.
			inside := v.start < end && v.end > start
			if v.start == v.end {
				inside = v.start >= start && v.start <= end
			}
			if inside {
				n.WriteString(Placeholder)
			}
			pos = v.end
		}
	}
	if n.Len() == 0 {
		return rawURL, "/"
	}
	return rawURL, n.String()
}

// writeInside writes the part of s, found at offset pos of the URL, that
// lies between start and end.
func writeInside(b *strings.Builder, s string, pos, start, end int) {
	from, to := max(start-pos, 0), min(end-pos, len(s))
	if from < to {
		b.WriteString(s[from:to])
	}
}
