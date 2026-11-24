---
title: "Start from a browser recording"
description: "Record a real session in your browser (the network tab can save it as a HAR file), then turn it into a scenario:"
sidebar_position: 15
---

# Start from a browser recording

Record a real session in your browser (the network tab can save it as a
HAR file), then turn it into a scenario:

```
vegaload import har recording.har -o checkout.vl.js
vegaload validate -secret-env VL_COOKIE checkout.vl.js
```

The importer makes a first draft, and you edit it. It keeps the real calls,
in the order they were recorded, one `http` call each, with a check on the
status the browser got. A call the browser got a status of 400 or higher for
lists it in `expectedStatuses`, so that answer is not counted as a failed
request. It leaves out images, fonts, style sheets and
scripts, requests to other sites (analytics, ads, CDNs), CORS preflight
requests, and requests that failed. "Other sites" means sites other than the
one of the first page you opened in the recording. Use `-include-static`,
`-include-third-party` or `-host` to change that, and `-max N` to stop after
N requests.

Each page of the recording is a `group()`, so the steps table has one row
per page. Each call gets a `name` with its path, and `{id}` for a part that
looks like an id, so its endpoint row is the same on every run, such as
`GET /api/products/{id}`. The waits between requests become `sleep()`
calls. `-think-time` says how:

- `recorded` (the default): the time from the end of one kept request to
  the start of the next. A request that was left out does not shorten it.
- `capped:5s`: as recorded, but at most 5 seconds.
- `fixed:1s`: 1 second before each request but the first.
- `none`: no sleeps.

A wait under 10 ms is not written. Sleep time is not in any call's latency.

```
vegaload import har -think-time capped:5s recording.har -o checkout.vl.js
```

A multipart form is rebuilt from the fields in the recording, with its own
boundary and a `multipart()` helper in the file.

The importer keeps the secrets it can recognise out of the file as
`env.VL_NAME`. You pass each one by name with `-secret-env`. That covers a
password, an API key, a session id, and any header, query parameter or body
field whose name looks secret. A `Cookie` or `Authorization` header is a
secret when an earlier response on the same host did not set that cookie or
return that bearer token. A CSRF token or a view state is not a secret: the
notes below say when the scenario reads it from a response. A value with an
ordinary name stays as it was recorded, including a token in a path such as
`/reset/<token>`, a query parameter named `code`, or a field named `key`.
Read the file before you share it, and do not commit the HAR file. The
importer also drops headers a client sets by itself (`User-Agent`,
`Content-Length`, `Referer`, `Origin`, `Sec-*` and similar).

Some things are left for you, and the file marks them with `TODO` lines:

- A value that looks like it changes on every run (a UUID, a long number, a
  token), when the nearest earlier response on the same host does not hold
  it in a named place. When it does, the scenario reads it from that
  response: a JSON field, then a response header, then a `Set-Cookie`, then
  an input found by `name` or `id`. Two places of the same kind, including
  two JSON fields, stay in the file with a `TODO`, and an older response is
  not used. A password or an API key stays in the environment even if a
  response echoes it. A CSRF token or a view state is carried when the page
  returned it, and stays in the file with a `TODO` when it did not.
- A file in a multipart form whose content is not in the recording. It
  gets a placeholder value.

The cookie jar sends back the cookies a server set. A cookie an earlier
response set is also copied into the `Cookie` header, so the scenario works
with `-no-cookies` too. Any other cookie is a secret. In a script,
`Set-Cookie` is every cookie the response set, separated by newlines, so a
date in `Expires` stays intact.

The hosts in the file are real. A host that is not localhost needs
`-allow-target` on the run, and the file's first lines say which.
