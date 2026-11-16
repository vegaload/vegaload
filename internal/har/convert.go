package har

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vegaload/vegaload/internal/urlname"
)

// part is a piece of a JavaScript string built from text and expressions.
// slot is 0, or one past the index in builder.slots. encode wraps a
// carried or secret value in encodeURIComponent.
type part struct {
	lit    string // literal text, when expr is empty
	expr   string // a JavaScript expression, such as env.VL_TOKEN
	slot   int
	encode bool
}

type header struct {
	name   string
	val    []part
	cookie bool
	auth   bool
}

// A body is one of: nothing, a JSON value (rendered with JSON.stringify),
// or a text made of parts.
type request struct {
	method      string
	url         []part
	headers     []header
	isJSON      bool // the body is JSON in jsonVal
	jsonVal     any
	text        []part // when the body is any other text
	hasBody     bool
	status      int
	label       string // METHOD /path, for messages
	page        string
	notes       []string // TODO lines
	answer      string   // text of the answer, to find where later values came from
	host        string
	respHeaders []nameValue
	cookies     []cookiePair
	emits       []emit // consts taken from this response
	name        string // the report row's name: the path, with {id}
	sleep       time.Duration
	multipart   *multipartBody
}

// multipartBody is a multipart form rebuilt from the recording.
type multipartBody struct {
	boundary string
	parts    []formPart
}

type formPart struct {
	name, fileName, contentType string
	value                       []part
}

// jsExpr is a raw JavaScript expression inside a JSON value.
type jsExpr string

type jKV struct {
	key string
	val any
}
type jObj []jKV
type jArr []any

var secretNames = []string{
	"password", "passwd", "secret", "token", "apikey", "credential",
	"session", "signature", "csrf", "xsrf", "jwt", "bearer", "cookie",
	"authorization", "accesskey", "privatekey",
}

// isSecretName reports whether a header, parameter or field name looks
// like it holds a secret. It is on the careful side: a wrong guess only
// means the scenario reads one more value from the environment.
func isSecretName(name string) bool {
	n := strings.ToLower(name)
	n = strings.NewReplacer("-", "", "_", "", " ", "", ".", "").Replace(n)
	switch n {
	case "auth", "pwd", "pass", "sid", "sig", "otp", "pin":
		return true
	}
	for _, s := range secretNames {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// alwaysSecretHeaders are secret whatever their value.
var alwaysSecretHeaders = map[string]bool{
	"authorization": true, "proxy-authorization": true, "cookie": true,
}

// dropHeaders are headers a client sets by itself, or that tie a request
// to the page where it was recorded.
var dropHeaders = map[string]bool{
	"host": true, "content-length": true, "connection": true, "keep-alive": true,
	"accept-encoding": true, "user-agent": true, "referer": true, "origin": true,
	"cache-control": true, "pragma": true, "upgrade-insecure-requests": true,
	"dnt": true, "te": true, "priority": true, "if-none-match": true,
	"if-modified-since": true, "if-match": true, "if-unmodified-since": true,
	"range": true, "if-range": true, "expect": true, "transfer-encoding": true,
	"upgrade": true, "accept-language": true,
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]+`)

// envName makes the environment variable name for a secret.
func envName(name string) string {
	s := strings.Trim(nonAlnum.ReplaceAllString(strings.ToUpper(name), "_"), "_")
	if s == "" {
		s = "SECRET"
	}
	return "VL_" + s
}

var (
	reUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	reJWT  = regexp.MustCompile(`^eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*$`)
	reHex  = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
	reNum  = regexp.MustCompile(`^[0-9]{8,}$`)
	reB64  = regexp.MustCompile(`^[A-Za-z0-9_-]{24,}={0,2}$`)
)

// dynamicKind says why a value looks like it changes on every run, or "".
func dynamicKind(v string) string {
	switch {
	case reUUID.MatchString(v):
		return "a UUID"
	case reJWT.MatchString(v):
		return "a JWT"
	case reHex.MatchString(v):
		return "a long hex value"
	case reNum.MatchString(v):
		return "a long number (an id or a timestamp?)"
	case reB64.MatchString(v) && hasLetterAndDigit(v):
		return "a long random-looking value"
	}
	return ""
}

func hasLetterAndDigit(s string) bool {
	var l, d bool
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			d = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			l = true
		}
	}
	return l && d
}

type builder struct {
	env   map[string]bool
	cur   int
	slots []slot
}

func (b *builder) secret(name string) part {
	n := envName(name)
	b.env[n] = true
	return part{expr: "env." + n}
}

// splitPairs splits "a=1&b=2" into raw name and value pairs, keeping the
// order and the original encoding.
func splitPairs(s string) [][2]string {
	if s == "" {
		return nil
	}
	var out [][2]string
	for _, p := range strings.Split(s, "&") {
		if p == "" {
			continue
		}
		k, v, _ := strings.Cut(p, "=")
		out = append(out, [2]string{k, v})
	}
	return out
}

func unesc(s string) string {
	if u, err := url.QueryUnescape(s); err == nil {
		return u
	}
	return s
}

// pairs renders name=value pairs as parts. A pair whose name looks secret
// reads its value from the environment.
func (b *builder) pairs(prs [][2]string) []part {
	var out []part
	for i, p := range prs {
		sep := ""
		if i > 0 {
			sep = "&"
		}
		name := unesc(p[0])
		decoded := unesc(p[1])
		if id, ok := b.consider(name, decoded); ok {
			out = append(out, part{lit: sep + p[0] + "="})
			out = append(out, part{slot: id + 1, encode: true, lit: p[1]})
			continue
		}
		if isSecretName(name) || reJWT.MatchString(decoded) {
			out = append(out, part{lit: sep + p[0] + "="})
			e := b.secret(name)
			e.expr = "encodeURIComponent(" + e.expr + ")"
			out = append(out, e)
			continue
		}
		out = append(out, part{lit: sep + p[0] + "=" + p[1]})
	}
	return out
}

func merge(ps []part) []part {
	var out []part
	for _, p := range ps {
		if p.expr == "" && p.slot == 0 && len(out) > 0 && out[len(out)-1].expr == "" && out[len(out)-1].slot == 0 {
			out[len(out)-1].lit += p.lit
			continue
		}
		if p.expr == "" && p.lit == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// buildURL renders a request URL. Secret-looking query values come from
// the environment, and path segments that look dynamic are flagged.
func (b *builder) buildURL(u *url.URL) []part {
	ps := []part{{lit: u.Scheme + "://" + u.Host}}
	raw := u.EscapedPath()
	if raw == "" {
		raw = "/"
	}
	for i, seg := range strings.Split(raw, "/") {
		if i > 0 {
			ps = append(ps, part{lit: "/"})
		}
		if seg == "" {
			continue
		}
		if id, ok := b.consider("", unesc(seg)); ok {
			ps = append(ps, part{slot: id + 1, encode: true, lit: seg})
			continue
		}
		ps = append(ps, part{lit: seg})
	}
	if u.RawQuery != "" {
		ps = append(ps, part{lit: "?"})
		ps = append(ps, b.pairs(splitPairs(u.RawQuery))...)
	}
	return merge(ps)
}

// parseOrdered reads JSON and keeps the order of object keys.
func parseOrdered(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := readValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("extra data after the JSON value")
	}
	return v, nil
}

func readValue(dec *json.Decoder) (any, error) {
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch tt := t.(type) {
	case json.Delim:
		switch tt {
		case '{':
			obj := jObj{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				v, err := readValue(dec)
				if err != nil {
					return nil, err
				}
				obj = append(obj, jKV{kt.(string), v})
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr := jArr{}
			for dec.More() {
				v, err := readValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
	}
	return t, nil // string, json.Number, bool, nil
}

// redactJSON replaces the value of every secret-looking field with an
// environment read, and flags values that look dynamic.
func (b *builder) redactJSON(v any) any {
	switch x := v.(type) {
	case jObj:
		out := make(jObj, len(x))
		for i, kv := range x {
			out[i] = jKV{kv.key, b.redactField(kv.key, kv.val)}
		}
		return out
	case jArr:
		out := make(jArr, len(x))
		for i, e := range x {
			out[i] = b.redactJSON(e)
		}
		return out
	}
	return v
}

func (b *builder) redactField(key string, v any) any {
	switch x := v.(type) {
	case jObj, jArr:
		return b.redactJSON(x)
	case string:
		return b.jsonLeaf(key, x, false)
	case json.Number:
		return b.jsonLeaf(key, x.String(), true)
	default:
		return v
	}
}

func (b *builder) jsonLeaf(key, val string, num bool) any {
	if id, ok := b.consider(key, val); ok {
		return pending{slot: id + 1, num: num, raw: val}
	}
	if isSecretName(key) || reJWT.MatchString(val) {
		return jsExpr(b.secret(key).expr)
	}
	if num {
		return json.Number(val)
	}
	return val
}

func (b *builder) headers(in []nameValue) []header {
	var out []header
	seen := map[string]bool{}
	for _, h := range in {
		n := strings.ToLower(h.Name)
		if n == "" || strings.HasPrefix(n, ":") || strings.HasPrefix(n, "sec-") || dropHeaders[n] || seen[n] {
			continue
		}
		seen[n] = true
		if strings.EqualFold(n, "cookie") {
			out = append(out, header{name: h.Name, cookie: true})
			continue
		}
		if strings.EqualFold(n, "authorization") {
			out = append(out, b.authHeader(h.Name, h.Value))
			continue
		}
		if alwaysSecretHeaders[n] || isSecretName(n) || reJWT.MatchString(h.Value) {
			out = append(out, header{name: h.Name, val: []part{b.secret(h.Name)}})
			continue
		}
		if id, ok := b.consider(h.Name, h.Value); ok {
			out = append(out, header{name: h.Name, val: []part{{slot: id + 1, lit: h.Value}}})
			continue
		}
		out = append(out, header{name: h.Name, val: []part{{lit: h.Value}}})
	}
	return out
}

func isJSONType(mime string) bool {
	m := strings.ToLower(mime)
	return strings.Contains(m, "json")
}

func isFormType(mime string) bool {
	return strings.HasPrefix(strings.ToLower(mime), "application/x-www-form-urlencoded")
}

func isMultipart(mime string) bool {
	return strings.HasPrefix(strings.ToLower(mime), "multipart/")
}

// Convert reads a HAR file and returns a scenario.
func Convert(r io.Reader, source string, opt Options) (*Result, error) {
	f, err := parse(r)
	if err != nil {
		return nil, err
	}
	res := &Result{Total: len(f.Log.Entries), Skipped: map[string]int{}}
	skip := func(why string) { res.Skipped[why]++ }

	// First pass: which entries are candidates, and which host is the main one.
	type cand struct {
		e *entry
		u *url.URL
	}
	var cands []cand
	for i := range f.Log.Entries {
		e := &f.Log.Entries[i]
		u, err := url.Parse(e.Request.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			skip(skipScheme)
			continue
		}
		if strings.EqualFold(e.Request.Method, "OPTIONS") {
			skip(skipPreflight)
			continue
		}
		if e.Response.Status == 0 {
			skip(skipFailed)
			continue
		}
		if !opt.IncludeStatic && isStatic(e, u) {
			skip(skipStatic)
			continue
		}
		cands = append(cands, cand{e, u})
	}

	keepHost := func(host string) (bool, string) {
		if len(opt.Hosts) > 0 {
			for _, h := range opt.Hosts {
				if strings.EqualFold(h, host) || strings.EqualFold(h, hostOnly(host)) {
					return true, ""
				}
			}
			return false, skipHost
		}
		return true, ""
	}
	// The main site is the one of the first page the person opened: the
	// first document in the recording, or the first request if the
	// recording has no resource types. Counting requests would pick an
	// analytics host that is called more often than the app.
	mainSite := ""
	if len(opt.Hosts) == 0 && !opt.IncludeThirdParty && len(cands) > 0 {
		first := cands[0]
		for _, c := range cands {
			if strings.EqualFold(c.e.ResourceType, "document") {
				first = c
				break
			}
		}
		mainSite = site(first.u.Host)
	}

	b := &builder{env: map[string]bool{}}
	hosts := map[string]bool{}
	var reqs []*request
	// prevEnd is when the last kept request ended. The requests left out
	// in between do not shorten the wait before the next kept one.
	var prevEnd time.Time
	for _, c := range cands {
		if ok, why := keepHost(c.u.Host); !ok {
			skip(why)
			continue
		}
		if mainSite != "" && site(c.u.Host) != mainSite {
			skip(skipThirdParty)
			continue
		}
		if opt.MaxRequests > 0 && len(reqs) >= opt.MaxRequests {
			skip(skipLimit)
			continue
		}
		q := b.request(len(reqs), c.e, c.u, f)
		started, timed := startTime(c.e)
		var gap time.Duration
		if timed && !prevEnd.IsZero() {
			gap = started.Sub(prevEnd)
		}
		q.sleep = opt.ThinkTime.wait(gap, len(reqs) == 0)
		if timed {
			prevEnd = started.Add(time.Duration(c.e.Time * float64(time.Millisecond)))
		}
		reqs = append(reqs, q)
		hosts[c.u.Host] = true
	}

	correlate(reqs, b.slots)
	applySlots(reqs, b)
	res.Requests = len(reqs)
	res.Hosts = sortedKeys(hosts)
	res.EnvNames = sortedKeys(b.env)
	for _, q := range reqs {
		res.Todos += len(q.notes)
	}
	res.Script = render(source, reqs, res, opt.ThinkTime)
	return res, nil
}

// startTime is when an entry started, if the recording says.
func startTime(e *entry) (time.Time, bool) {
	if e.StartedDateTime == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, e.StartedDateTime)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// request converts one entry.
func (b *builder) request(idx int, e *entry, u *url.URL, f *file) *request {
	b.cur = idx
	q := &request{method: strings.ToUpper(e.Request.Method), status: e.Response.Status, host: u.Host}
	q.url = b.buildURL(u)
	q.cookies = b.cookieSlots(cookieValue(e.Request.Headers))
	q.headers = b.headers(e.Request.Headers)
	q.label = q.method + " " + u.Path
	if u.Path == "" {
		q.label = q.method + " /"
	}
	q.name = urlname.Fold(urlname.Path(e.Request.URL))
	q.page = e.Pageref
	for _, p := range f.Log.Pages {
		if p.ID == e.Pageref && p.Title != "" {
			q.page = p.Title
		}
	}
	if q.method != "GET" && q.method != "HEAD" {
		b.body(q, e)
	}
	q.respHeaders = e.Response.Headers
	q.answer = responseText(e)
	return q
}

func shorten(s string) string {
	if len(s) > 28 {
		return `"` + s[:25] + `..."`
	}
	return `"` + s + `"`
}

func (b *builder) body(q *request, e *entry) {
	pd := e.Request.PostData
	if pd == nil {
		return
	}
	mime := pd.MimeType
	switch {
	case isMultipart(mime):
		mp := b.multipart(q, pd.MimeType, pd.Text, pd.Params)
		if mp == nil {
			q.notes = append(q.notes, "the body was a multipart form that could not be read. It was left out. Build it by hand.")
			return
		}
		q.multipart = mp
		q.hasBody = true
		setHeader(q, "Content-Type", "multipart/form-data; boundary="+mp.boundary)
	case pd.Text == "" && len(pd.Params) > 0 && isFormType(mime):
		var prs [][2]string
		for _, p := range pd.Params {
			prs = append(prs, [2]string{url.QueryEscape(p.Name), url.QueryEscape(p.Value)})
		}
		q.text = merge(b.pairs(prs))
		q.hasBody = true
	case pd.Text == "":
		return
	case isJSONType(mime) || looksJSON(pd.Text):
		v, err := parseOrdered([]byte(pd.Text))
		if err == nil {
			q.jsonVal = b.redactJSON(v)
			q.isJSON = true
			q.hasBody = true
			return
		}
		fallthrough
	case isFormType(mime):
		if isFormType(mime) {
			q.text = merge(b.pairs(splitPairs(pd.Text)))
		} else {
			q.text = []part{{lit: pd.Text}}
		}
		q.hasBody = true
	default:
		q.text = []part{{lit: pd.Text}}
		q.hasBody = true
	}
}

// multipart rebuilds a multipart form from its recorded fields, or from the
// recorded text when there are none. It returns nil when neither can be
// read. A file whose content the recording does not have gets a
// placeholder and a TODO.
func (b *builder) multipart(q *request, mimeType, text string, params []param) *multipartBody {
	if len(params) == 0 {
		params = readMultipart(mimeType, text)
		if params == nil {
			return nil
		}
	}
	mp := &multipartBody{boundary: fmt.Sprintf("vegaload-form-%d", b.cur+1)}
	for _, p := range params {
		fp := formPart{name: p.Name, fileName: p.FileName, contentType: p.ContentType}
		switch {
		case p.FileName != "" && (p.Value == "" || !utf8.ValidString(p.Value)):
			fp.value = []part{{lit: "REPLACE WITH THE CONTENT OF " + p.FileName}}
			q.notes = append(q.notes, fmt.Sprintf("the file %q of the form field %q is not in the recording. Put its content in value.", p.FileName, p.Name))
		case p.FileName == "" && isSecretName(p.Name):
			fp.value = []part{b.secret(p.Name)}
		default:
			fp.value = []part{{lit: p.Value}}
		}
		mp.parts = append(mp.parts, fp)
	}
	return mp
}

// readMultipart reads the fields of a recorded multipart text, or nil.
func readMultipart(mimeType, text string) []param {
	_, mt, err := mime.ParseMediaType(mimeType)
	if err != nil || mt["boundary"] == "" || text == "" {
		return nil
	}
	r := multipart.NewReader(strings.NewReader(text), mt["boundary"])
	var out []param
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil
		}
		body, err := io.ReadAll(p)
		if err != nil {
			return nil
		}
		out = append(out, param{Name: p.FormName(), Value: string(body), FileName: p.FileName(), ContentType: p.Header.Get("Content-Type")})
	}
	return out
}

// setHeader sets a request header, replacing one of the same name.
func setHeader(q *request, name, value string) {
	for i, h := range q.headers {
		if strings.EqualFold(h.name, name) {
			q.headers[i].val = []part{{lit: value}}
			return
		}
	}
	q.headers = append(q.headers, header{name: name, val: []part{{lit: value}}})
}

func looksJSON(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")
}
