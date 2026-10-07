// Package har turns a HAR file (the recording a browser's network tab can
// save) into a VegaLoad JavaScript scenario. It is for the first draft of
// a scenario: record a real session in the browser, import it, then edit
// the file.
//
// What the importer does to a recording:
//
//   - It drops what a load test should not repeat: images, fonts, style
//     sheets and scripts; requests to other sites (analytics, ads, CDNs);
//     CORS preflight requests; and requests that failed in the browser.
//   - It keeps the secrets it can recognise out of the scenario. A Cookie or
//     Authorization header, any header, query parameter or body field whose
//     name looks secret, and any value that is a JWT, is read from the
//     environment at run time (env.VL_NAME). The scenario lists the
//     variables to pass with -secret-env. The check works on names and on
//     the shape of a JWT. A secret with an ordinary name stays in the file
//     as it was recorded: a token in a path such as /reset/<token>, a query
//     parameter named code, or a field named key. Read the file before you
//     share it, and do not commit the HAR file itself.
//   - It drops headers that a client sets by itself (User-Agent, Host,
//     Content-Length, Accept-Encoding and similar), and the headers that
//     tie a request to the recorded page (Origin, Referer, Sec-*).
//   - It marks values that look like they change on every run, such as a
//     UUID, a long number or a token, with a TODO comment. When the same
//     value appears in the answer to an earlier request, the comment says
//     which request, so the author can carry the value forward.
//
// It does not guess a flow: it keeps the recorded order and one call per
// request. It writes no waits between calls, because the scenario API has
// no sleep. It does not import WebSocket frames or multipart bodies; it
// says so in a TODO where that happens.
package har

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"path"
	"sort"
	"strings"
)

// Options controls what Convert keeps.
type Options struct {
	// Hosts keeps only requests to these hosts (a name, or name:port).
	// When empty, Convert keeps the site of the first page that the
	// recording opened (its first document request).
	Hosts []string
	// IncludeStatic keeps images, fonts, style sheets and scripts.
	IncludeStatic bool
	// IncludeThirdParty keeps requests to hosts of other sites.
	IncludeThirdParty bool
	// MaxRequests stops after this many requests. 0 means no limit.
	MaxRequests int
}

// Result is the scenario and a summary of what was done.
type Result struct {
	Script   string         // the scenario file content
	Requests int            // requests in the scenario
	Total    int            // entries in the recording
	Skipped  map[string]int // why entries were left out, with counts
	Hosts    []string       // hosts the scenario calls, sorted
	EnvNames []string       // environment variables the scenario reads, sorted
	Todos    int            // TODO notes in the scenario
}

type nameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type entry struct {
	Pageref      string `json:"pageref"`
	ResourceType string `json:"_resourceType"`
	Request      struct {
		Method   string      `json:"method"`
		URL      string      `json:"url"`
		Headers  []nameValue `json:"headers"`
		PostData *struct {
			MimeType string      `json:"mimeType"`
			Text     string      `json:"text"`
			Params   []nameValue `json:"params"`
		} `json:"postData"`
	} `json:"request"`
	Response struct {
		Status  int `json:"status"`
		Content struct {
			MimeType string `json:"mimeType"`
			Text     string `json:"text"`
			Encoding string `json:"encoding"`
		} `json:"content"`
	} `json:"response"`
}

type page struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type file struct {
	Log struct {
		Pages   []page  `json:"pages"`
		Entries []entry `json:"entries"`
	} `json:"log"`
}

// Skip reasons, as they are counted and shown to the user.
const (
	skipStatic     = "static file (image, font, style sheet or script)"
	skipThirdParty = "another site"
	skipHost       = "not in -host"
	skipPreflight  = "CORS preflight (OPTIONS)"
	skipFailed     = "failed in the browser (no response)"
	skipScheme     = "not an http or https request"
	skipLimit      = "over -max"
)

// parse reads a HAR file.
func parse(r io.Reader) (*file, error) {
	var f file
	dec := json.NewDecoder(r)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("not a HAR file: %w", err)
	}
	if f.Log.Entries == nil {
		return nil, fmt.Errorf("not a HAR file: no log.entries")
	}
	return &f, nil
}

var staticExt = map[string]bool{
	".js": true, ".mjs": true, ".css": true, ".map": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true, ".ico": true,
	".webp": true, ".avif": true, ".bmp": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	".mp3": true, ".mp4": true, ".webm": true, ".ogg": true, ".wav": true,
}

// isStatic reports whether an entry is a file a browser loads for a page
// and a load test would not repeat.
func isStatic(e *entry, u *url.URL) bool {
	switch strings.ToLower(e.ResourceType) {
	case "image", "stylesheet", "script", "font", "media", "texttrack", "manifest":
		return true
	case "xhr", "fetch", "document", "websocket":
		return false
	}
	mt := strings.ToLower(e.Response.Content.MimeType)
	if i := strings.Index(mt, ";"); i >= 0 {
		mt = mt[:i]
	}
	mt = strings.TrimSpace(mt)
	switch {
	case strings.HasPrefix(mt, "image/"), strings.HasPrefix(mt, "font/"),
		strings.HasPrefix(mt, "audio/"), strings.HasPrefix(mt, "video/"),
		mt == "text/css", mt == "text/javascript", mt == "application/javascript",
		mt == "application/x-javascript", mt == "application/font-woff",
		mt == "application/x-font-woff", mt == "application/vnd.ms-fontobject":
		return true
	}
	return staticExt[strings.ToLower(path.Ext(u.Path))]
}

// site is a rough "same site" key: the last two labels of the host, or
// the last three when the second to last is a short generic label such as
// co in example.co.uk. It is a heuristic, not the public suffix list, and
// -host overrides it.
func site(host string) string {
	h := hostOnly(host)
	if net.ParseIP(strings.Trim(h, "[]")) != nil {
		return h // an address has no site above itself
	}
	parts := strings.Split(h, ".")
	if len(parts) <= 2 {
		return h
	}
	n := 2
	switch parts[len(parts)-2] {
	case "co", "com", "org", "net", "gov", "edu", "ac":
		if len(parts[len(parts)-1]) == 2 {
			n = 3
		}
	}
	if len(parts) < n {
		return h
	}
	return strings.Join(parts[len(parts)-n:], ".")
}

func hostOnly(host string) string {
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i:], "]") {
		return host[:i]
	}
	return host
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// responseText returns an entry's answer as text, for finding where a
// value came from. Large and binary answers give "".
func responseText(e *entry) string {
	c := e.Response.Content
	if c.Text == "" {
		return ""
	}
	mt := strings.ToLower(c.MimeType)
	if !(strings.Contains(mt, "json") || strings.Contains(mt, "text") || strings.Contains(mt, "xml") || strings.Contains(mt, "html")) {
		return ""
	}
	text := c.Text
	if strings.EqualFold(c.Encoding, "base64") {
		b, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return ""
		}
		text = string(b)
	}
	if len(text) > 1<<20 {
		return ""
	}
	return text
}
