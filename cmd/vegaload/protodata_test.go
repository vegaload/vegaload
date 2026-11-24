package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/protocol/ftp/ftptest"
	"github.com/vegaload/vegaload/internal/protocol/kafka/kafkatest"
	"github.com/vegaload/vegaload/internal/protocol/mysql/mysqltest"
	"github.com/vegaload/vegaload/internal/protocol/postgres/postgrestest"
	"github.com/vegaload/vegaload/internal/protocol/rabbitmq/rabbitmqtest"
	"github.com/vegaload/vegaload/internal/protocol/redis/redistest"
	"github.com/vegaload/vegaload/internal/protocol/sftp/sftptest"
	"github.com/vegaload/vegaload/internal/report"
	"github.com/vegaload/vegaload/internal/scripting/netapi"
)

func TestProtocolData_ClassifiesEveryOption(t *testing.T) {
	for _, d := range drivers {
		class, ok := protocolDataClass[d.name]
		if d.perCall == nil {
			if ok {
				t.Errorf("%s is classified but has no per-call adapter", d.name)
			}
			continue
		}
		if !ok {
			t.Errorf("%s can take -data but its options are not classified", d.name)
			continue
		}
		got := map[string]bool{}
		for _, k := range class.placeholder {
			if got[k] {
				t.Errorf("%s lists %s twice", d.name, k)
			}
			got[k] = true
		}
		for _, k := range class.fixed {
			if got[k] {
				t.Errorf("%s lists %s as both a placeholder and a fixed option", d.name, k)
			}
			got[k] = true
		}
		if len(got) != len(d.options) {
			t.Errorf("%s classifies %d options, the driver has %d", d.name, len(got), len(d.options))
		}
		for _, k := range d.options {
			if !got[k] {
				t.Errorf("%s option %s is not classified", d.name, k)
			}
		}
		if (d.name == "postgres" || d.name == "mysql") && class.body {
			t.Errorf("%s must refuse a placeholder in the SQL text", d.name)
		}
		if d.name != "postgres" && d.name != "mysql" && !class.body {
			t.Errorf("%s should allow a placeholder in -body", d.name)
		}
	}
}

func TestProtocolData_UsageErrors(t *testing.T) {
	users := writeCSV(t, "users.csv", "id,note\nabc,hi\n")
	good := writeCSV(t, "people.csv", "id,note\n1,hi\n")
	extra := writeCSV(t, "spare.csv", "id\n1\n")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"placeholder in mode", []string{"-data", good, "-target", "ftp://127.0.0.1:1", "-protocol", "ftp", "-opt", "mode={data.people.id}", "-opt", "path=/x"}, "mode"},
		{"placeholder in password_env", []string{"-data", good, "-target", "postgres://127.0.0.1:1/db", "-protocol", "postgres", "-opt", "password_env={data.people.note}", "-opt", "args=[{data.people.id:int}]", "-body", "select $1"}, "password_env"},
		{"placeholder in allow_writes", []string{"-data", good, "-target", "ftp://127.0.0.1:1", "-protocol", "ftp", "-opt", "mode=upload", "-opt", "allow_writes={data.people.note}", "-opt", "path=/{data.people.note}"}, "allow_writes"},
		{"placeholder in a TLS option", []string{"-data", good, "-target", "postgres://127.0.0.1:1/db", "-protocol", "postgres", "-opt", "ca_file={data.people.note}", "-opt", "args=[{data.people.id:int}]", "-body", "select $1"}, "ca_file"},
		{"sql body", []string{"-data", good, "-target", "postgres://127.0.0.1:1/db", "-protocol", "postgres", "-body", "select {data.people.note}", "-opt", "sslmode=disable"}, "put the value in -opt args"},
		{"unused set", []string{"-data", good, "-data", extra, "-target", "redis://127.0.0.1:1", "-protocol", "redis", "-body", "GET {data.people.id}"}, "-data spare: nothing uses it"},
		{"missing column", []string{"-data", good, "-target", "redis://127.0.0.1:1", "-protocol", "redis", "-body", "GET {data.people.city}"}, "people.csv"},
		{"missing file", []string{"-data", filepath.Join(t.TempDir(), "nope.csv"), "-target", "redis://127.0.0.1:1", "-protocol", "redis", "-body", "GET {data.nope.id}"}, "nope.csv"},
		{"mqtt", []string{"-data", good, "-target", "mqtt://127.0.0.1:1", "-protocol", "mqtt", "-opt", "mode=publish", "-body", "hi"}, "not supported with -protocol mqtt"},
		{"bad pick", []string{"-data-pick", "shuffle", "-data", good, "-target", "redis://127.0.0.1:1", "-protocol", "redis", "-body", "PING"}, "next or random"},
		{"pick without data", []string{"-data-pick", "random", "-target", "redis://127.0.0.1:1", "-protocol", "redis", "-body", "PING"}, "-data-pick needs -data"},
		{"env with protocol", []string{"-env", "HOME", "-target", "redis://127.0.0.1:1", "-protocol", "redis", "-body", "PING"}, "only -data and -data-pick"},
		{"first row is not an integer", []string{"-data", users, "-target", "postgres://127.0.0.1:1/db", "-protocol", "postgres", "-opt", "args=[{data.users.id:int}]", "-body", "select $1"}, "not an integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRunArgs(append([]string{"-no-report"}, tc.args...))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "abc") && tc.name == "first row is not an integer" {
				t.Fatalf("the error shows the cell: %v", err)
			}
			if code := cmdRun(append([]string{"-no-report", "-audit-log", filepath.Join(t.TempDir(), "a.log")}, tc.args...)); code != 2 {
				t.Fatalf("exit %d, want 2", code)
			}
		})
	}
}

func TestProtocolData_PostgresQuotesArgs(t *testing.T) {
	const cell = "a\"b\\c café"
	srv := postgrestest.Start(t, func(string) []postgrestest.Result {
		return []postgrestest.Result{{Columns: []postgrestest.Column{postgrestest.Text("name")}, Rows: [][]any{{"ann"}}}}
	})
	csv := writeCSV(t, "users.csv", "id,note\n7,\""+strings.ReplaceAll(cell, `"`, `""`)+"\"\n")
	dir := t.TempDir()
	out := filepath.Join(dir, "out.json")
	auditPath := filepath.Join(dir, "audit.log")
	code := cmdRun([]string{
		"-vus", "1", "-duration", "200ms", "-no-report", "-audit-log", auditPath, "-out", out,
		"-target", srv.URL(), "-protocol", "postgres", "-data", csv,
		"-opt", "sslmode=disable",
		"-opt", "args=[{data.users.id:int},{data.users.note}]",
		"-body", "SELECT name FROM users WHERE id = $1 AND note = $2",
	})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	queries := srv.Queries()
	if len(queries) == 0 {
		t.Fatal("no queries")
	}
	for _, q := range queries {
		if !strings.Contains(q, "id =  7 ") || !strings.Contains(q, "E'a\"b\\\\c café'") {
			t.Fatalf("query %q", q)
		}
		if strings.Contains(q, "{data") || strings.Contains(q, "$1") || strings.Contains(q, "$2") {
			t.Fatalf("the SQL template was not left for the driver: %q", q)
		}
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var res report.Result
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.DataFiles) != 1 || res.DataFiles[0].Name != "users.csv" || res.DataFiles[0].Rows != 1 || strings.Contains(string(raw), cell) {
		t.Fatalf("report = %s", raw)
	}
	auditRaw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(auditRaw), "users.csv") || strings.Contains(string(auditRaw), cell) {
		t.Fatalf("audit = %s", auditRaw)
	}
}

func TestProtocolData_PostgresJSONNumber(t *testing.T) {
	srv := postgrestest.Start(t, func(string) []postgrestest.Result {
		return []postgrestest.Result{{Columns: []postgrestest.Column{postgrestest.Int("id")}, Rows: [][]any{{"1"}}}}
	})
	js := writeCSV(t, "users.json", "[{\"id\": 42}]")
	iter, cleanup := openDataIter(t, nil,
		"-timeout", "2s", "-no-report",
		"-target", srv.URL(), "-protocol", "postgres", "-data", js,
		"-opt", "sslmode=disable", "-opt", "args=[{data.users.id}]",
		"-body", "SELECT name FROM users WHERE id = $1",
	)
	defer cleanup()
	if err := iter(context.Background()); err != nil {
		t.Fatal(err)
	}
	q := srv.Queries()
	if len(q) != 1 || !strings.Contains(q[0], " 42 ") || strings.Contains(q[0], "'42'") {
		t.Fatalf("query %q", q)
	}
}

func TestProtocolData_MySQLArgs(t *testing.T) {
	srv := mysqltest.Start(t, func(string) []mysqltest.Result {
		return []mysqltest.Result{{Columns: []mysqltest.Column{mysqltest.Text("name")}, Rows: [][]any{{"ann"}}}}
	})
	csv := writeCSV(t, "users.csv", "id\n7\n")
	iter, cleanup := openDataIter(t, nil,
		"-timeout", "2s", "-no-report",
		"-target", srv.URL(), "-protocol", "mysql", "-data", csv,
		"-opt", "args=[{data.users.id:int}]",
		"-body", "SELECT name FROM users WHERE id = ?",
	)
	defer cleanup()
	if err := iter(context.Background()); err != nil {
		t.Fatal(err)
	}
	q := srv.Queries()
	if len(q) != 1 || !strings.Contains(q[0], "id =  7 ") || strings.Contains(q[0], "{data") || strings.Contains(q[0], "?") {
		t.Fatalf("query %q", q)
	}
}

func TestProtocolData_RedisWordAndSpread(t *testing.T) {
	const cell = "a\"b\\c café"
	srv := redistest.Start(t, func(args []string) redistest.Value { return redistest.SimpleString("OK") })
	csv := writeCSV(t, "keys.csv", "token\n\""+strings.ReplaceAll(cell, `"`, `""`)+"\"\n")
	iter, cleanup := openDataIter(t, nil,
		"-timeout", "2s", "-no-report",
		"-target", srv.URL(), "-protocol", "redis", "-data", csv,
		"-body", "GET {data.keys.token}",
	)
	defer cleanup()
	if err := iter(context.Background()); err != nil {
		t.Fatal(err)
	}
	cmds := srv.Commands()
	if len(cmds) != 1 || len(cmds[0]) != 2 || cmds[0][0] != "GET" || cmds[0][1] != cell {
		t.Fatalf("commands %#v", cmds)
	}

	var b strings.Builder
	b.WriteString("id\n")
	for i := 0; i < 25; i++ {
		b.WriteString(idName(i) + "\n")
	}
	ids := writeCSV(t, "ids.csv", b.String())
	srv2 := redistest.Start(t, func(args []string) redistest.Value { return redistest.SimpleString("OK") })
	iter, cleanup = openDataIter(t, nil,
		"-timeout", "2s", "-no-report",
		"-target", srv2.URL(), "-protocol", "redis", "-data", ids, "-data-pick", "next",
		"-body", "GET {data.ids.id}",
	)
	defer cleanup()
	ctx := context.Background()
	for round := 0; round < 25; round++ {
		before := len(srv2.Commands())
		var wg sync.WaitGroup
		errCh := make(chan error, 4)
		for n := 0; n < 4; n++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := iter(ctx); err != nil {
					errCh <- err
				}
			}()
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatal(err)
		}
		got := srv2.Commands()[before:]
		if len(got) != 4 {
			t.Fatalf("round %d: %d commands", round, len(got))
		}
		seen := map[string]bool{}
		for _, c := range got {
			if len(c) != 2 || seen[c[1]] {
				t.Fatalf("round %d: %#v", round, got)
			}
			seen[c[1]] = true
		}
	}
	count := map[string]int{}
	for _, c := range srv2.Commands() {
		count[c[1]]++
	}
	if len(count) != 25 {
		t.Fatalf("used %d keys", len(count))
	}
	for k, n := range count {
		if n != 4 {
			t.Fatalf("key %s used %d times", k, n)
		}
	}

	srv3 := redistest.Start(t, func(args []string) redistest.Value { return redistest.SimpleString("OK") })
	iter, cleanup = openDataIter(t, nil,
		"-timeout", "2s", "-no-report",
		"-target", srv3.URL(), "-protocol", "redis", "-data", ids, "-data-pick", "random",
		"-body", "GET {data.ids.id}",
	)
	defer cleanup()
	for i := 0; i < 40; i++ {
		if err := iter(ctx); err != nil {
			t.Fatal(err)
		}
	}
	allowed := map[string]bool{}
	for i := 0; i < 25; i++ {
		allowed[idName(i)] = true
	}
	for _, c := range srv3.Commands() {
		if !allowed[c[1]] {
			t.Fatalf("random used %q", c[1])
		}
	}
}

func TestProtocolData_RedisRace(t *testing.T) {
	srv := redistest.Start(t, func(args []string) redistest.Value { return redistest.SimpleString("OK") })
	csv := writeCSV(t, "keys.csv", "token\na\nb\nc\n")
	iter, cleanup := openDataIter(t, nil,
		"-timeout", "2s", "-no-report",
		"-target", srv.URL(), "-protocol", "redis", "-data", csv,
		"-body", "GET {data.keys.token}",
	)
	defer cleanup()
	var wg sync.WaitGroup
	errCh := make(chan error, 20)
	for n := 0; n < 20; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if err := iter(context.Background()); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if len(srv.Commands()) != 200 {
		t.Fatalf("commands %d", len(srv.Commands()))
	}
}

func TestProtocolData_SFTPPath(t *testing.T) {
	// The test server turns a backslash into a slash on Windows (pkg/sftp
	// does filepath.ToSlash on the path), so keep the backslash off Windows.
	cell := "a\"b\\c café"
	if runtime.GOOS == "windows" {
		cell = "a\"b c café"
	}
	srv := sftptest.Start(t)
	path := "/export/" + cell
	srv.Apply(sftptest.Config{Files: map[string][]byte{path: []byte("hi")}})
	t.Setenv("SFTP_PW", "secret")
	csv := writeCSV(t, "files.csv", "name\n\""+strings.ReplaceAll(cell, `"`, `""`)+"\"\n")
	iter, cleanup := openDataIter(t, nil,
		"-timeout", "3s", "-no-report", "-insecure",
		"-target", srv.URL(), "-protocol", "sftp", "-data", csv,
		"-opt", "password_env=SFTP_PW", "-opt", "mode=download",
		"-opt", "path=/export/{data.files.name}",
	)
	defer cleanup()
	if err := iter(context.Background()); err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, op := range srv.Ops() {
		if strings.Contains(op, path) {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("ops %q", srv.Ops())
	}
}

func TestProtocolData_FTPWriteStaysRefused(t *testing.T) {
	srv := ftptest.Start(t)
	csv := writeCSV(t, "files.csv", "name\nup.bin\n")
	col := report.NewCollector()
	iter, cleanup := openDataIter(t, col,
		"-timeout", "2s", "-no-report",
		"-target", srv.URL(), "-protocol", "ftp", "-data", csv,
		"-opt", "mode=upload", "-opt", "path=/{data.files.name}", "-opt", "size=1",
	)
	defer cleanup()
	err := iter(context.Background())
	if err == nil {
		t.Fatal("upload without allow_writes succeeded")
	}
	res := col.Finish("fixed-vus", time.Second)
	if res.Total != 0 {
		t.Fatalf("a refused call was recorded: %+v", res)
	}
	for _, c := range srv.Commands() {
		if strings.Contains(c, "STOR") {
			t.Fatalf("sent %q", c)
		}
	}
}

func TestProtocolData_FTPPathLineBreak(t *testing.T) {
	srv := ftptest.Start(t)
	srv.Apply(ftptest.Config{Files: map[string][]byte{"/ok": []byte("x")}})
	csv := writeCSV(t, "files.csv", "name\nok\n\"CRLFMARKER\nmore\"\n")
	col := report.NewCollector()
	iter, cleanup := openDataIter(t, col,
		"-timeout", "2s", "-no-report",
		"-target", srv.URL(), "-protocol", "ftp", "-data", csv,
		"-opt", "mode=download", "-opt", "path=/{data.files.name}",
	)
	defer cleanup()
	if err := iter(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := iter(context.Background())
	if err == nil || !strings.Contains(err.Error(), "line break") || strings.Contains(err.Error(), "CRLFMARKER") {
		t.Fatalf("err = %v", err)
	}
	got := col.Finish("fixed-vus", time.Second)
	p, ok := got.Protocol("ftp")
	if !ok || p.Total != 2 || p.Failed != 1 {
		t.Fatalf("recorded %+v", p)
	}
}

func TestProtocolData_IntFailureIsAFailedCall(t *testing.T) {
	srv := postgrestest.Start(t, func(string) []postgrestest.Result {
		return []postgrestest.Result{{Columns: []postgrestest.Column{postgrestest.Int("n")}, Rows: [][]any{{"1"}}}}
	})
	csv := writeCSV(t, "users.csv", "id\n1\nabc\n")
	col := report.NewCollector()
	iter, cleanup := openDataIter(t, col,
		"-timeout", "2s", "-no-report",
		"-target", srv.URL(), "-protocol", "postgres", "-data", csv,
		"-opt", "sslmode=disable", "-opt", "args=[{data.users.id:int}]",
		"-body", "select $1",
	)
	defer cleanup()
	if err := iter(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := iter(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not an integer") || strings.Contains(err.Error(), "abc") || !strings.Contains(err.Error(), "users.csv") || !strings.Contains(err.Error(), "-opt args") {
		t.Fatalf("err = %v", err)
	}
	got := col.Finish("fixed-vus", time.Second)
	p, ok := got.Protocol("postgres")
	if !ok || p.Total != 2 || p.Failed != 1 {
		t.Fatalf("recorded %+v", p)
	}
}

func TestProtocolData_FTPKeepsID(t *testing.T) {
	srv := ftptest.Start(t)
	csv := writeCSV(t, "files.csv", "name\nnote\n")
	iter, cleanup := openDataIter(t, nil,
		"-timeout", "2s", "-no-report",
		"-target", srv.URL(), "-protocol", "ftp", "-data", csv,
		"-opt", "mode=upload", "-opt", "allow_writes=true",
		"-opt", "path=/up-{id}-{data.files.name}.bin", "-opt", "size=1",
	)
	defer cleanup()
	for i := 0; i < 2; i++ {
		if err := iter(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var stores []string
	for _, c := range srv.Commands() {
		if strings.Contains(c, " STOR ") {
			stores = append(stores, c)
		}
	}
	if len(stores) != 2 || stores[0] == stores[1] || strings.Contains(stores[0], "{id}") || strings.Contains(stores[0], "{data") || !strings.Contains(stores[0], "note") {
		t.Fatalf("stores %q", stores)
	}
}

func TestProtocolData_KafkaBody(t *testing.T) {
	b := kafkatest.Start(t)
	b.AddTopic("orders", 1)
	csv := writeCSV(t, "orders.csv", "sku\nsku-9\n")
	iter, cleanup := openDataIter(t, nil,
		"-timeout", "3s", "-no-report",
		"-target", b.URL(), "-protocol", "kafka", "-data", csv,
		"-opt", "topic=orders", "-body", "{data.orders.sku}",
	)
	defer cleanup()
	if err := iter(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(b.Values("orders")) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := b.Values("orders"); len(got) != 1 || got[0] != "sku-9" {
		t.Fatalf("values %#v produced %d", got, b.ProducedCount())
	}
}

func TestProtocolData_RabbitMQ(t *testing.T) {
	srv := rabbitmqtest.Start(t)
	csv := writeCSV(t, "orders.csv", "key,note\nk1,hello\n")
	iter, cleanup := openDataIter(t, nil,
		"-timeout", "3s", "-no-report",
		"-target", srv.URL(), "-protocol", "rabbitmq", "-data", csv,
		"-opt", "exchange=amq.topic", "-opt", "mandatory=false",
		"-opt", "routing_key={data.orders.key}", "-body", "{data.orders.note}",
	)
	defer cleanup()
	if err := iter(context.Background()); err != nil {
		t.Fatal(err)
	}
	pubs := srv.Published()
	if len(pubs) != 1 || pubs[0].RoutingKey != "k1" || string(pubs[0].Body) != "hello" {
		t.Fatalf("published %+v", pubs)
	}
}

func TestProtocolData_NoDataJSONUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	dir := t.TempDir()
	out := filepath.Join(dir, "out.json")
	code := cmdRun([]string{
		"-vus", "1", "-duration", "150ms", "-no-report",
		"-audit-log", filepath.Join(dir, "audit.log"), "-out", out,
		"-target", srv.URL, "-protocol", "http1",
	})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "data_files") {
		t.Fatalf("a run without -data recorded data files: %s", raw)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want, err := os.ReadFile("testdata/no-data-json-keys.txt")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, "\n")+"\n" != string(want) {
		t.Fatalf("keys\n%s\nwant\n%s", strings.Join(keys, "\n"), want)
	}
	auditRaw, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(auditRaw), "data_files") {
		t.Fatalf("audit = %s", auditRaw)
	}
}

func TestPrintAndReportMentionDataFilesOnlyWhenPresent(t *testing.T) {
	var buf strings.Builder
	printResult(&buf, &report.Result{Executor: "fixed-vus"})
	if strings.Contains(buf.String(), "data files") {
		t.Fatalf("summary mentioned data files:\n%s", buf.String())
	}
	printResult(&buf, &report.Result{Executor: "fixed-vus", DataFiles: []report.DataFile{{Name: "users.csv", Rows: 3}}})
	if !strings.Contains(buf.String(), "users.csv  3 rows") {
		t.Fatalf("summary = %s", buf.String())
	}
	if out := report.MarkdownSummary(&report.Result{Executor: "fixed-vus"}); strings.Contains(out, "Data files") {
		t.Fatal(out)
	}
	if out := report.MarkdownSummary(&report.Result{Executor: "fixed-vus", DataFiles: []report.DataFile{{Name: "users.csv", Rows: 3}}}); !strings.Contains(out, "users.csv (3 rows)") {
		t.Fatal(out)
	}
}

func openDataIter(t *testing.T, calls netapi.CallRecorder, args ...string) (func(context.Context) error, func()) {
	t.Helper()
	cfg, err := parseRunArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	iter, closeFn, err := protocolIteration(cfg.Protocol, cfg.Target, cfg.Timeout, calls, cfg.DataPlan, false)
	if err != nil {
		t.Fatal(err)
	}
	return iter, func() { _ = closeFn() }
}

func writeCSV(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func idName(i int) string { return fmt.Sprintf("k%02d", i) }
