package har

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// jsString returns s as a JavaScript string literal.
func jsString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimRight(buf.String(), "\n")
}

func renderParts(ps []part) string {
	if len(ps) == 0 {
		return `""`
	}
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if p.expr != "" {
			out = append(out, p.expr)
		} else {
			out = append(out, jsString(p.lit))
		}
	}
	return strings.Join(out, " + ")
}

// renderValue writes a JSON value as a JavaScript literal. indent is the
// indentation of the line that holds the value.
func renderValue(v any, indent string) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case json.Number:
		return x.String()
	case string:
		return jsString(x)
	case jsExpr:
		return string(x)
	case jArr:
		if len(x) == 0 {
			return "[]"
		}
		in := indent + "  "
		var sb strings.Builder
		sb.WriteString("[\n")
		for _, e := range x {
			sb.WriteString(in + renderValue(e, in) + ",\n")
		}
		sb.WriteString(indent + "]")
		return sb.String()
	case jObj:
		if len(x) == 0 {
			return "{}"
		}
		in := indent + "  "
		var sb strings.Builder
		sb.WriteString("{\n")
		for _, kv := range x {
			sb.WriteString(in + jsString(kv.key) + ": " + renderValue(kv.val, in) + ",\n")
		}
		sb.WriteString(indent + "}")
		return sb.String()
	}
	return jsString(fmt.Sprint(v))
}

var verbFuncs = map[string]string{
	"GET": "get", "POST": "post", "PUT": "put", "PATCH": "patch", "DELETE": "delete",
}

// commentLines writes text as // comments, wrapped to about 76 columns.
func commentLines(prefix, text string) string {
	return commentLinesFrom(prefix, prefix, text)
}

// commentLinesFrom is commentLines with a different prefix for the lines
// after the first.
func commentLinesFrom(first, rest, text string) string {
	var sb strings.Builder
	line, prefix := first, first
	for _, w := range strings.Fields(text) {
		if len(line)+len(w)+1 > 76 && line != prefix {
			sb.WriteString(line + "\n")
			line, prefix = rest, rest
		}
		if line != prefix {
			line += " "
		}
		line += w
	}
	sb.WriteString(line + "\n")
	return sb.String()
}

func render(source string, reqs []*request, res *Result, think ThinkTime) string {
	var sb strings.Builder
	w := sb.WriteString

	w("// A VegaLoad scenario made from " + source + " by `vegaload import har`.\n")
	w("//\n")
	w("// This is a first draft. Read it, edit it, then check it with\n")
	w("// `vegaload validate` before you load it.\n")
	w("//\n")
	w(fmt.Sprintf("// %d of %d recorded requests are here, in the order they were recorded.\n", res.Requests, res.Total))
	if len(res.Skipped) > 0 {
		keys := make([]string, 0, len(res.Skipped))
		for k := range res.Skipped {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		w("// Left out:\n")
		for _, k := range keys {
			w(fmt.Sprintf("//   %d: %s\n", res.Skipped[k], k))
		}
	}
	w("//\n")
	if len(res.Hosts) > 0 {
		w("// Hosts called. A host that is not localhost needs -allow-target (or -yes):\n")
		var flags []string
		for _, h := range res.Hosts {
			flags = append(flags, "-allow-target "+h)
		}
		w(commentLines("//   ", strings.Join(flags, " ")))
		w("//\n")
	}
	if len(res.EnvNames) > 0 {
		w("// Secrets are not in this file. The scenario reads them from the\n")
		w("// environment. Set each one in your shell and pass it by name:\n")
		var flags []string
		for _, n := range res.EnvNames {
			flags = append(flags, "-secret-env "+n)
		}
		w(commentLines("//   ", strings.Join(flags, " ")))
		w("//\n")
	}
	w("// A value that the nearest earlier response on the same host held in a\n")
	w("// named place is read from that response. When that response holds it\n")
	w("// in several kinds of place, a JSON field wins, then a response header,\n")
	w("// then a Set-Cookie, then an input. Two places of the same kind stay a\n")
	w("// TODO, and an older response is not used. Anything else that looks\n")
	w("// like it changes on every run is a TODO. A cookie an earlier response\n")
	w("// did not set is still a secret. The cookie jar sends back the cookies\n")
	w("// a server set, so the copies below are only needed with -no-cookies.\n")
	w("//\n")
	grouped := false
	for _, q := range reqs {
		grouped = grouped || q.page != ""
	}
	if grouped {
		w("// Each page of the recording is a group(), and each call's report row\n")
		w("// is named after its path, with {id} for a part that looks like an id.\n")
	} else {
		w("// Each call's report row is named after its path, with {id} for a part\n")
		w("// that looks like an id.\n")
	}
	switch think.Mode {
	case ThinkNone:
		w("// No waits are written (-think-time none).\n")
	default:
		w("// The sleep() calls are the waits between requests (-think-time " + think.String() + ").\n")
		w("// Sleep time is not in any call's latency.\n")
	}
	w("\n")

	if len(res.EnvNames) > 0 {
		var names []string
		for _, n := range res.EnvNames {
			names = append(names, jsString(n))
		}
		w("const SECRETS = [" + strings.Join(names, ", ") + "];\n\n")
	}
	w("// want is the status the browser got. 0 means any answer below 400.\n")
	w("function expectStatus(res, want, label) {\n")
	w("  const bad = want === 0 ? res.status >= 400 : res.status !== want;\n")
	w("  if (bad) throw new Error(label + \": status \" + res.status + \", the recording had \" + (want || \"below 400\"));\n")
	w("}\n\n")
	w(helperSource(reqs))

	w("export default function () {\n")
	if len(res.EnvNames) > 0 {
		w("  for (const name of SECRETS) {\n")
		w("    if (env[name] === undefined) throw new Error(\"set \" + name + \" and pass -secret-env \" + name);\n")
		w("  }\n\n")
	}
	var carried []string
	for _, q := range reqs {
		for _, e := range q.emits {
			carried = append(carried, e.name)
		}
	}
	// A value carried from one page to a later one must outlive its group.
	hoist := grouped && len(carried) > 0
	if hoist {
		w(commentLinesFrom("  let ", "    ", strings.Join(carried, ", ")+";"))
		w("\n")
	}
	page, open := "", false
	for i, q := range reqs {
		n := i + 1
		if grouped && (q.page != page || i == 0) {
			if open {
				w("  });\n\n")
			}
			w(sleepLine("  ", q.sleep))
			page, open = q.page, q.page != ""
			if open {
				w("  group(" + jsString(oneLine(q.page)) + ", () => {\n")
			}
		} else {
			w(indent(sleepLine("  ", q.sleep), open))
		}
		w(indent(renderRequest(n, q, hoist), open))
	}
	if open {
		w("  });\n")
	}
	w("}\n")
	return sb.String()
}

// renderRequest is one request: its comment, call, status check and the
// values later requests read from it.
func renderRequest(n int, q *request, hoisted bool) string {
	var sb strings.Builder
	w := sb.WriteString
	want := q.status
	if want >= 300 && want < 400 {
		want = 0
	}
	w(fmt.Sprintf("  // %d. %s (the recording had %d)\n", n, oneLine(q.label), q.status))
	for _, note := range q.notes {
		w(commentLinesFrom("  // TODO: ", "  //       ", note))
	}
	w(renderCall(n, q))
	w(fmt.Sprintf("  expectStatus(r%d, %d, %s);\n", n, want, jsString(q.label)))
	for _, e := range q.emits {
		if hoisted {
			w(fmt.Sprintf("  %s = %s;\n", e.name, e.expr))
		} else {
			w(fmt.Sprintf("  const %s = %s;\n", e.name, e.expr))
		}
	}
	w("\n")
	return sb.String()
}

// sleepLine is the wait before a request, or "".
func sleepLine(prefix string, d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return prefix + "sleep(" + strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + ");\n"
}

// indent moves every line of s two spaces in, when in is true.
func indent(s string, in bool) string {
	if !in || s == "" {
		return s
	}
	lines := strings.SplitAfter(s, "\n")
	for i, l := range lines {
		if l != "" && l != "\n" {
			lines[i] = "  " + l
		}
	}
	return strings.Join(lines, "")
}

func helperSource(reqs []*request) string {
	var header, cookie, hidden, form bool
	for _, q := range reqs {
		form = form || q.multipart != nil
		for _, e := range q.emits {
			header = header || strings.Contains(e.expr, "header(")
			cookie = cookie || strings.Contains(e.expr, "cookie(")
			hidden = hidden || strings.Contains(e.expr, "hidden(")
		}
	}
	if cookie {
		header = true
	}
	var sb strings.Builder
	if header {
		sb.WriteString(helperHeader)
	}
	if cookie {
		sb.WriteString(helperCookie)
	}
	if hidden {
		sb.WriteString(helperHidden)
	}
	if form {
		sb.WriteString(helperMultipart)
	}
	return sb.String()
}

const helperMultipart = `// multipart builds a multipart/form-data body. Send it with the header
// "Content-Type: multipart/form-data; boundary=" + boundary.
function multipart(boundary, parts) {
  let body = "";
  for (const p of parts) {
    body += "--" + boundary + "\r\n";
    body += "Content-Disposition: form-data; name=\"" + p.name + "\"";
    if (p.fileName !== undefined) body += "; filename=\"" + p.fileName + "\"";
    body += "\r\n";
    if (p.contentType) body += "Content-Type: " + p.contentType + "\r\n";
    body += "\r\n" + p.value + "\r\n";
  }
  return body + "--" + boundary + "--\r\n";
}

`

const helperHeader = `function header(res, name) {
  const want = name.toLowerCase();
  for (const key of Object.keys(res.headers)) {
    if (key.toLowerCase() === want) return res.headers[key];
  }
  return "";
}

`

const helperCookie = `function cookie(setCookie, name) {
  const prefix = name + "=";
  for (const part of String(setCookie).split("\n")) {
    const first = part.split(";")[0].trim();
    if (first.startsWith(prefix)) return first.slice(prefix.length);
  }
  throw new Error("no cookie " + name);
}

`

const helperHidden = `function hidden(body, name) {
  // Stops at the first ">". An attribute value that itself contains ">" is missed.
  const re = /<input\b[^>]*>/gi;
  let m;
  while ((m = re.exec(body))) {
    if (attr(m[0], "name") === name || attr(m[0], "id") === name) return attr(m[0], "value");
  }
  throw new Error("no input " + name);
}
function attr(tag, name) {
  const want = name.toLowerCase();
  let i = 0;
  while (i < tag.length) {
    while (i < tag.length && tag[i] !== " " && tag[i] !== "\t" && tag[i] !== "\n" && tag[i] !== "/") i++;
    while (i < tag.length && (tag[i] === " " || tag[i] === "\t" || tag[i] === "\n" || tag[i] === "/")) i++;
    let j = i;
    while (j < tag.length && tag[j] !== "=" && tag[j] !== " " && tag[j] !== ">") j++;
    const key = tag.slice(i, j).toLowerCase();
    while (j < tag.length && tag[j] !== "=" && tag[j] !== ">") j++;
    if (j >= tag.length || tag[j] !== "=") { i = j + 1; continue; }
    j++;
    while (j < tag.length && tag[j] === " ") j++;
    let v = "";
    if (tag[j] === '"' || tag[j] === "'") {
      const q = tag[j];
      j++;
      const k = tag.indexOf(q, j);
      v = k < 0 ? tag.slice(j) : tag.slice(j, k);
      i = k < 0 ? tag.length : k + 1;
    } else {
      const k = j;
      while (j < tag.length && tag[j] !== " " && tag[j] !== ">") j++;
      v = tag.slice(k, j);
      i = j;
    }
    if (key === want) return decodeAttr(v);
  }
  return "";
}
function decodeAttr(s) {
  return s.replace(/&amp;/g, "&").replace(/&quot;/g, "\"").replace(/&#39;|&apos;/g, "'").replace(/&lt;/g, "<").replace(/&gt;/g, ">");
}

`

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func renderCall(n int, q *request) string {
	var opts []string
	if len(q.headers) > 0 {
		var hs []string
		for _, h := range q.headers {
			hs = append(hs, "      "+jsString(h.name)+": "+renderParts(h.val)+",\n")
		}
		opts = append(opts, "    headers: {\n"+strings.Join(hs, "")+"    },\n")
	}
	if q.hasBody {
		if q.multipart != nil {
			opts = append(opts, "    body: "+renderMultipart(q.multipart)+",\n")
		} else if q.isJSON {
			opts = append(opts, "    body: JSON.stringify("+renderValue(q.jsonVal, "    ")+"),\n")
		} else {
			opts = append(opts, "    body: "+renderParts(q.text)+",\n")
		}
	}
	if q.status >= 400 {
		// Without it, the answer the recording had would count as a failed request.
		opts = append(opts, fmt.Sprintf("    expectedStatuses: [%d],\n", q.status))
	}
	if q.name != "" {
		opts = append(opts, "    name: "+jsString(q.name)+",\n")
	}
	var sb strings.Builder
	url := renderParts(q.url)
	if fn, ok := verbFuncs[q.method]; ok {
		sb.WriteString(fmt.Sprintf("  const r%d = http.%s(%s", n, fn, url))
	} else {
		sb.WriteString(fmt.Sprintf("  const r%d = http.request(%s, %s", n, jsString(q.method), url))
	}
	if len(opts) > 0 {
		sb.WriteString(", {\n" + strings.Join(opts, "") + "  }")
	}
	sb.WriteString(");\n")
	return sb.String()
}

// renderMultipart is the multipart(...) call that builds a form's body.
func renderMultipart(mp *multipartBody) string {
	var sb strings.Builder
	sb.WriteString("multipart(" + jsString(mp.boundary) + ", [\n")
	for _, p := range mp.parts {
		fields := []string{"name: " + jsString(p.name)}
		if p.fileName != "" {
			fields = append(fields, "fileName: "+jsString(p.fileName))
		}
		if p.contentType != "" {
			fields = append(fields, "contentType: "+jsString(p.contentType))
		}
		fields = append(fields, "value: "+renderParts(p.value))
		sb.WriteString("      { " + strings.Join(fields, ", ") + " },\n")
	}
	sb.WriteString("    ])")
	return sb.String()
}
