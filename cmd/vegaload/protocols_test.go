package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vegaload/vegaload/internal/mcp"
	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/protocol/ftp/ftptest"
	"github.com/vegaload/vegaload/internal/protocol/grpc/grpctest"
	"github.com/vegaload/vegaload/internal/protocol/mysql/mysqltest"
	"github.com/vegaload/vegaload/internal/protocol/postgres/postgrestest"
	"github.com/vegaload/vegaload/internal/protocol/rabbitmq/rabbitmqtest"
	"github.com/vegaload/vegaload/internal/protocol/redis/redistest"
	"github.com/vegaload/vegaload/internal/protocol/sftp/sftptest"
	"github.com/vegaload/vegaload/internal/report"
)

func TestJoinOr(t *testing.T) {
	cases := []struct {
		names []string
		want  string
	}{
		{nil, ""},
		{[]string{"a"}, "a"},
		{[]string{"a", "b"}, "a or b"},
		{[]string{"a", "b", "c"}, "a, b, or c"},
	}
	for _, c := range cases {
		if got := joinOr(c.names); got != c.want {
			t.Errorf("joinOr(%v) = %q, want %q", c.names, got, c.want)
		}
	}
}

// The drivers table is the only place a protocol is wired, so every entry
// must build, and the help text must list exactly the table's names.
func TestDrivers_EveryEntryBuilds(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range drivers {
		if seen[d.name] {
			t.Errorf("protocol %q is listed twice", d.name)
		}
		seen[d.name] = true

		iter, closeFn, err := protocolIteration(d.name, wireTestTarget(d.name), 0, nil, nil, false)
		if err != nil {
			t.Errorf("protocol %q does not build: %v", d.name, err)
			continue
		}
		if iter == nil || closeFn == nil {
			t.Errorf("protocol %q returned a nil iteration or close", d.name)
			continue
		}
		_ = closeFn()
	}
	for _, name := range protocolNames() {
		if !strings.Contains(protocolList(), name) {
			t.Errorf("help text %q does not list %q", protocolList(), name)
		}
	}
}

func TestProtocolIteration_UnknownProtocol(t *testing.T) {
	_, _, err := protocolIteration("nope", protocol.Target{URL: "x"}, 0, nil, nil, false)
	if err == nil || !strings.Contains(err.Error(), "unknown -protocol") || !strings.Contains(err.Error(), "http1") {
		t.Fatalf("err = %v, want it to name the choices", err)
	}
}

// A typo in -opt is an error, not ignored. This holds for every driver:
// a key it did not declare is refused.
func TestProtocolIteration_UnknownOptionIsRefused(t *testing.T) {
	for _, d := range drivers {
		tg := wireTestTarget(d.name)
		tg.Options = map[string]string{"definitely-not-a-key": "1"}
		_, _, err := protocolIteration(d.name, tg, 0, nil, nil, false)
		if err == nil || !strings.Contains(err.Error(), "unknown option definitely-not-a-key") {
			t.Errorf("protocol %q: err = %v, want an unknown option error", d.name, err)
		}
	}
}

func TestProtocolIteration_DriverWithoutOptionsSaysSo(t *testing.T) {
	tg := wireTestTarget("http1")
	tg.Options = map[string]string{"read": "64"}
	_, _, err := protocolIteration("http1", tg, 0, nil, nil, false)
	if err == nil || !strings.Contains(err.Error(), "unknown option read") {
		t.Fatalf("err = %v, want an unknown option error", err)
	}
}

// The MCP run_test tool describes the protocols in its schema. That text
// lives in another package, so this test keeps it in step with the table.
func TestMCPRunTestSchema_ListsEveryProtocol(t *testing.T) {
	var desc string
	for _, tool := range mcp.NewTools("vegaload") {
		if tool.Name != "run_test" {
			continue
		}
		props := tool.InputSchema["properties"].(map[string]any)
		desc = props["protocol"].(map[string]any)["description"].(string)
	}
	if desc == "" {
		t.Fatal("run_test has no protocol description")
	}
	for _, name := range protocolNames() {
		if !strings.Contains(desc, name) {
			t.Errorf("run_test protocol description %q does not mention %q", desc, name)
		}
	}
}

// wireTestTarget returns a target address the named driver accepts at
// construction time. Nothing connects: New only checks the address.
func wireTestTarget(name string) protocol.Target {
	switch name {
	case "grpc":
		return protocol.Target{URL: "localhost:1", Method: "/p.S/M"}
	case "websocket":
		return protocol.Target{URL: "ws://localhost:1/"}
	case "mqtt":
		return protocol.Target{URL: "mqtt://localhost:1", Options: map[string]string{"topic": "t"}}
	case "kafka":
		return protocol.Target{URL: "kafka://localhost:1", Options: map[string]string{"topic": "t"}}
	case "postgres":
		return protocol.Target{URL: "postgres://localhost:1/db", Body: []byte("select 1")}
	case "mysql":
		return protocol.Target{URL: "mysql://localhost:1/db", Body: []byte("select 1")}
	case "redis":
		return protocol.Target{URL: "redis://localhost:1", Body: []byte("PING")}
	case "rabbitmq":
		return protocol.Target{URL: "amqp://localhost:1", Body: []byte("hello")}
	case "ftp":
		return protocol.Target{URL: "ftp://localhost:1", Options: map[string]string{"mode": "connect"}}
	case "sftp":
		// New reads the password here. This helper has no testing.T.
		_ = os.Setenv("VEGALOAD_WIRE_SFTP_PW", "wire")
		return protocol.Target{
			URL:     "sftp://load@localhost:1",
			Options: map[string]string{"mode": "connect", "password_env": "VEGALOAD_WIRE_SFTP_PW"},
		}
	case "tcp":
		return protocol.Target{URL: "tcp://localhost:1"}
	case "udp":
		return protocol.Target{URL: "udp://localhost:1", Body: []byte("x")}
	}
	return protocol.Target{URL: "http://localhost:1/"}
}

// A real option of a driver is accepted by the command's check.
func TestProtocolIteration_KnownOptionIsAccepted(t *testing.T) {
	tg := wireTestTarget("tcp")
	tg.Options = map[string]string{"read": "4", "expect": "ok"}
	if _, closeFn, err := protocolIteration("tcp", tg, 0, nil, nil, false); err != nil {
		t.Fatalf("known tcp options refused: %v", err)
	} else {
		_ = closeFn()
	}

	tg = wireTestTarget("udp")
	tg.Options = map[string]string{"read": "4"}
	if _, _, err := protocolIteration("udp", tg, 0, nil, nil, false); err == nil {
		t.Error("read is a tcp option and must be refused for udp")
	}
}

// A scenario that mixes HTTP and gRPC runs end to end through `vegaload run`
// (FR-CLI-19).
func TestCmdRun_ScenarioCallsGRPC(t *testing.T) {
	srv := grpctest.Start(t, true)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"service":"up"}`))
	}))
	t.Cleanup(api.Close)

	dir := t.TempDir()
	scenario := filepath.Join(dir, "mixed.vl.js")
	src := `export default function () {
  const cfg = step("http", () => http.get("` + api.URL + `").json());
  step("grpc", () => {
    const r = grpc.call("` + srv.Addr + `", {method: "/grpc.health.v1.Health/Check", body: {service: cfg.service}});
    check(r, {"serving": (x) => x.ok && x.json.status === "SERVING"});
    if (!r.ok) throw new Error(r.error);
  });
}`
	if err := os.WriteFile(scenario, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "summary.json")
	code := cmdRun([]string{
		"-vus", "2", "-duration", "300ms", "-no-report",
		"-audit-log", filepath.Join(dir, "audit.log"), "-out", out,
		"-threshold", "check_rate >= 100%", "-threshold", `error_rate{step="grpc"} < 1%`,
		scenario,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var res report.Result
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	if s, ok := res.Step("grpc"); !ok || s.Total == 0 || s.Failed != 0 {
		t.Errorf("grpc step = %+v, %v", s, ok)
	}
}

// A PostgreSQL load test runs end to end through `vegaload run -protocol
// postgres`, and its checks (min_rows) fail the iteration when not met.
func TestCmdRun_PostgresProtocol(t *testing.T) {
	srv := postgrestest.Start(t, func(string) []postgrestest.Result {
		return []postgrestest.Result{{
			Columns: []postgrestest.Column{postgrestest.Int("n")},
			Rows:    [][]any{{"1"}, {"2"}},
		}}
	})
	run := func(minRows string) report.Result {
		dir := t.TempDir()
		out := filepath.Join(dir, "summary.json")
		cmdRun([]string{
			"-vus", "2", "-duration", "300ms", "-no-report",
			"-audit-log", filepath.Join(dir, "audit.log"), "-out", out,
			"-target", srv.URL(), "-protocol", "postgres", "-body", "select n from t",
			"-opt", "sslmode=disable", "-opt", "pool=2", "-opt", "min_rows=" + minRows,
		})
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		var res report.Result
		if err := json.Unmarshal(data, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := run("2"); res.Total == 0 || res.Failed != 0 {
		t.Errorf("min_rows=2: total %d failed %d, want all to pass", res.Total, res.Failed)
	}
	if res := run("3"); res.Total == 0 || res.Failed != res.Total {
		t.Errorf("min_rows=3: total %d failed %d, want all to fail", res.Total, res.Failed)
	}
	if n := srv.ConnCount(); n > 4 {
		t.Errorf("%d connections for two runs with pool=2, want the pool reused", n)
	}
}

// A MySQL load test runs end to end through `vegaload run -protocol mysql`,
// and its checks (min_rows) fail the iteration when not met.
func TestCmdRun_MySQLProtocol(t *testing.T) {
	srv := mysqltest.Start(t, func(string) []mysqltest.Result {
		return []mysqltest.Result{{
			Columns: []mysqltest.Column{mysqltest.Int("n")},
			Rows:    [][]any{{"1"}, {"2"}},
		}}
	})
	run := func(minRows string) report.Result {
		dir := t.TempDir()
		out := filepath.Join(dir, "summary.json")
		cmdRun([]string{
			"-vus", "2", "-duration", "300ms", "-no-report",
			"-audit-log", filepath.Join(dir, "audit.log"), "-out", out,
			"-target", srv.URL(), "-protocol", "mysql", "-body", "select n from t",
			"-opt", "tls=false", "-opt", "pool=2", "-opt", "min_rows=" + minRows,
		})
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		var res report.Result
		if err := json.Unmarshal(data, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := run("2"); res.Total == 0 || res.Failed != 0 {
		t.Errorf("min_rows=2: total %d failed %d, want all to pass", res.Total, res.Failed)
	}
	if res := run("3"); res.Total == 0 || res.Failed != res.Total {
		t.Errorf("min_rows=3: total %d failed %d, want all to fail", res.Total, res.Failed)
	}
	if n := srv.ConnCount(); n > 4 {
		t.Errorf("%d connections for two runs with pool=2, want the pool reused", n)
	}
}

// A Redis load test runs end to end through `vegaload run -protocol redis`,
// and its checks (min_rows) fail the iteration when not met.
func TestCmdRun_RedisProtocol(t *testing.T) {
	srv := redistest.Start(t, func(args []string) redistest.Value {
		return redistest.ArrayOf(redistest.BulkString("1"), redistest.BulkString("2"))
	})
	run := func(minRows string) report.Result {
		dir := t.TempDir()
		out := filepath.Join(dir, "summary.json")
		cmdRun([]string{
			"-vus", "2", "-duration", "300ms", "-no-report",
			"-audit-log", filepath.Join(dir, "audit.log"), "-out", out,
			"-target", srv.URL(), "-protocol", "redis", "-body", "LRANGE k 0 -1",
			"-opt", "pool=2", "-opt", "min_rows=" + minRows,
		})
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		var res report.Result
		if err := json.Unmarshal(data, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := run("2"); res.Total == 0 || res.Failed != 0 {
		t.Errorf("min_rows=2: total %d failed %d, want all to pass", res.Total, res.Failed)
	}
	if res := run("3"); res.Total == 0 || res.Failed != res.Total {
		t.Errorf("min_rows=3: total %d failed %d, want all to fail", res.Total, res.Failed)
	}
}

// A RabbitMQ load test runs end to end through `vegaload run -protocol rabbitmq`
// for each mode: publish, consume, roundtrip and admin.
func TestCmdRun_RabbitMQProtocol(t *testing.T) {
	srv := rabbitmqtest.Start(t)
	run := func(extra ...string) report.Result {
		dir := t.TempDir()
		out := filepath.Join(dir, "summary.json")
		args := []string{
			"-executor", "constant-arrival-rate", "-rate", "150", "-max-vus", "4", "-duration", "300ms", "-no-report",
			"-audit-log", filepath.Join(dir, "audit.log"), "-out", out,
			"-target", srv.URL(), "-protocol", "rabbitmq",
		}
		args = append(args, extra...)
		cmdRun(args)
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		var res report.Result
		if err := json.Unmarshal(data, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := run("-opt", "mode=admin", "-opt", "action=queue_declare", "-opt", "queue=q", "-opt", "allow_writes=true", "-body", "x"); res.Total == 0 || res.Failed != 0 {
		t.Errorf("queue_declare: total %d failed %d", res.Total, res.Failed)
	}
	if res := run("-opt", "routing_key=q", "-body", "hello"); res.Total == 0 || res.Failed != 0 {
		t.Errorf("publish: total %d failed %d", res.Total, res.Failed)
	}
	if res := run("-opt", "mode=consume", "-opt", "queue=q", "-body", "x"); res.Total == 0 || res.Failed != 0 {
		t.Errorf("consume: total %d failed %d", res.Total, res.Failed)
	}
	if res := run("-opt", "mode=roundtrip", "-body", "hello"); res.Total == 0 || res.Failed != 0 {
		t.Errorf("roundtrip: total %d failed %d", res.Total, res.Failed)
	}
}

// An FTP load test runs end to end through `vegaload run -protocol ftp`
// for each mode: connect, download, list, stat, upload, roundtrip and delete.
func TestCmdRun_FTPProtocol(t *testing.T) {
	srv := ftptest.Start(t)
	srv.Apply(ftptest.Config{Files: map[string][]byte{
		"/pub/data.bin": []byte("hello ftp"),
		"/pub/gone.bin": []byte("bye"),
	}})
	run := func(extra ...string) report.Result {
		dir := t.TempDir()
		out := filepath.Join(dir, "summary.json")
		// Paced: these modes open a socket for each call. A free-running loop opens
		// thousands, and on Windows that uses up the local ports for later tests.
		args := []string{
			"-executor", "constant-arrival-rate", "-rate", "150", "-max-vus", "4", "-duration", "300ms", "-no-report",
			"-audit-log", filepath.Join(dir, "audit.log"), "-out", out,
			"-target", srv.URL(), "-protocol", "ftp",
		}
		args = append(args, extra...)
		cmdRun(args)
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		var res report.Result
		if err := json.Unmarshal(data, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := run("-opt", "mode=connect"); res.Total == 0 || res.Failed != 0 {
		t.Errorf("connect: total %d failed %d", res.Total, res.Failed)
	}
	if res := run("-opt", "mode=download", "-opt", "path=/pub/data.bin", "-opt", "expect_size=9"); res.Total == 0 || res.Failed != 0 {
		t.Errorf("download: total %d failed %d", res.Total, res.Failed)
	} else if p, ok := res.Protocol("ftp"); !ok || len(res.Protocols) != 1 || p.Total != res.Total || len(p.Operations) != 1 || p.Operations[0].Name != "download" || p.Operations[0].Total != res.Total {
		t.Errorf("download protocol = %+v ok %v, run total %d", p, ok, res.Total)
	}
	if res := run("-opt", "mode=list", "-opt", "path=/pub", "-opt", "expect=data.bin"); res.Total == 0 || res.Failed != 0 {
		t.Errorf("list: total %d failed %d", res.Total, res.Failed)
	}
	if res := run("-opt", "mode=stat", "-opt", "path=/pub/data.bin"); res.Total == 0 || res.Failed != 0 {
		t.Errorf("stat: total %d failed %d", res.Total, res.Failed)
	}
	if res := run("-opt", "mode=upload", "-opt", "path=/up-{id}.bin", "-opt", "size=1KiB", "-opt", "allow_writes=true"); res.Total == 0 || res.Failed != 0 {
		t.Errorf("upload: total %d failed %d", res.Total, res.Failed)
	}
	if res := run("-opt", "mode=roundtrip", "-opt", "path=/rt-{id}.bin", "-opt", "size=1KiB", "-opt", "allow_writes=true"); res.Total == 0 || res.Failed != 0 {
		t.Errorf("roundtrip: total %d failed %d", res.Total, res.Failed)
	} else {
		assertPhaseCounts(t, res)
	}
	res := run("-opt", "mode=delete", "-opt", "path=/pub/gone.bin", "-opt", "allow_writes=true", "-opt", "allow_admin=true")
	if res.Total == 0 {
		t.Errorf("delete: total %d", res.Total)
	}
	if _, ok := srv.File("/pub/gone.bin"); ok {
		t.Error("delete left /pub/gone.bin")
	}
}

// An SFTP load test runs end to end through `vegaload run -protocol sftp`
// for each mode: connect, download, list, stat, upload, roundtrip and delete.
func TestCmdRun_SFTPProtocol(t *testing.T) {
	srv := sftptest.Start(t)
	srv.Apply(sftptest.Config{Files: map[string][]byte{
		"/pub/data.bin": []byte("hello sftp"),
		"/pub/gone.bin": []byte("bye"),
	}})
	t.Setenv("SFTP_PW", "secret")
	run := func(extra ...string) report.Result {
		dir := t.TempDir()
		out := filepath.Join(dir, "summary.json")
		args := []string{
			"-executor", "constant-arrival-rate", "-rate", "150", "-max-vus", "4", "-duration", "300ms", "-no-report", "-insecure",
			"-audit-log", filepath.Join(dir, "audit.log"), "-out", out,
			"-target", srv.URL(), "-protocol", "sftp",
			"-opt", "password_env=SFTP_PW",
		}
		args = append(args, extra...)
		cmdRun(args)
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		var res report.Result
		if err := json.Unmarshal(data, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := run("-opt", "mode=connect"); res.Total == 0 || res.Failed != 0 {
		t.Errorf("connect: total %d failed %d", res.Total, res.Failed)
	}
	if res := run("-opt", "mode=download", "-opt", "path=/pub/data.bin", "-opt", "expect_size=10"); res.Total == 0 || res.Failed != 0 {
		t.Errorf("download: total %d failed %d", res.Total, res.Failed)
	}
	if res := run("-opt", "mode=list", "-opt", "path=/pub", "-opt", "expect=data.bin"); res.Total == 0 || res.Failed != 0 {
		t.Errorf("list: total %d failed %d", res.Total, res.Failed)
	}
	if res := run("-opt", "mode=stat", "-opt", "path=/pub/data.bin"); res.Total == 0 || res.Failed != 0 {
		t.Errorf("stat: total %d failed %d", res.Total, res.Failed)
	}
	if res := run("-opt", "mode=upload", "-opt", "path=/up-{id}.bin", "-opt", "size=1KiB", "-opt", "allow_writes=true"); res.Total == 0 || res.Failed != 0 {
		t.Errorf("upload: total %d failed %d", res.Total, res.Failed)
	}
	if res := run("-opt", "mode=roundtrip", "-opt", "path=/rt-{id}.bin", "-opt", "size=1KiB", "-opt", "allow_writes=true"); res.Total == 0 || res.Failed != 0 {
		t.Errorf("roundtrip: total %d failed %d", res.Total, res.Failed)
	} else {
		assertPhaseCounts(t, res)
	}
	res := run("-opt", "mode=delete", "-opt", "path=/pub/gone.bin", "-opt", "allow_writes=true", "-opt", "allow_admin=true")
	if res.Total == 0 {
		t.Errorf("delete: total %d", res.Total)
	}
	if _, ok := srv.File("/pub/gone.bin"); ok {
		t.Error("delete left /pub/gone.bin")
	}
}

// A scenario that calls HTTP, PostgreSQL and FTP records three protocols,
// in the order the script first called them.
func TestCmdRun_ScenarioRecordsProtocols(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(api.Close)
	pg := postgrestest.Start(t, func(string) []postgrestest.Result {
		return []postgrestest.Result{{
			Columns: []postgrestest.Column{postgrestest.Int("n")},
			Rows:    [][]any{{"1"}},
		}}
	})
	ftpSrv := ftptest.Start(t)

	jsSrc := `export default function () {
  http.get("` + api.URL + `");
  const pg = postgres.query("` + pg.URL() + `", {sslmode: "disable", body: "select 1"});
  if (!pg.ok) throw new Error(pg.error);
  const rt = ftp.roundtrip("` + ftpSrv.URL() + `", {path: "/rt-{id}", size: "8", allow_writes: true});
  if (!rt.ok) throw new Error(rt.error);
}
`
	pySrc := `def iteration():
    http.get("` + api.URL + `")
    pg = postgres.query("` + pg.URL() + `", sslmode="disable", body="select 1")
    if not pg.ok:
        raise Exception(pg.error)
    rt = ftp.roundtrip("` + ftpSrv.URL() + `", path="/rt-{id}", size="8", allow_writes=True)
    if not rt.ok:
        raise Exception(rt.error)
`
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"mixed.vl.js", jsSrc},
		{"mixed.py", pySrc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			scenario := filepath.Join(dir, tc.name)
			if err := os.WriteFile(scenario, []byte(tc.src), 0o644); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "summary.json")
			code := cmdRun([]string{
				"-vus", "1", "-duration", "400ms", "-no-report",
				"-audit-log", filepath.Join(dir, "audit.log"), "-out", out,
				scenario,
			})
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			var res report.Result
			if err := json.Unmarshal(data, &res); err != nil {
				t.Fatal(err)
			}
			if len(res.Protocols) != 3 {
				t.Fatalf("protocols = %+v", recordedNames(res.Protocols))
			}
			want := []string{"http", "postgres", "ftp"}
			for i, name := range want {
				if res.Protocols[i].Name != name || res.Protocols[i].Total == 0 || res.Protocols[i].Failed != 0 {
					t.Errorf("protocol %d = %+v, want %s", i, res.Protocols[i], name)
				}
			}
			var uploads int64
			for _, m := range res.Protocols[2].Measures {
				if m.Name == "upload_time" {
					uploads = m.Count
				}
			}
			if uploads == 0 {
				t.Errorf("ftp measures = %+v", res.Protocols[2].Measures)
			}
		})
	}
}

// A successful roundtrip records every phase, including one that finished
// in under a millisecond. The count matches the calls.
func assertPhaseCounts(t *testing.T, res report.Result) {
	t.Helper()
	if len(res.Protocols) != 1 {
		t.Fatalf("protocols = %+v", res.Protocols)
	}
	p := res.Protocols[0]
	counts := map[string]int64{}
	for _, m := range p.Measures {
		counts[m.Name] = m.Count
	}
	for _, name := range []string{"upload_time", "download_time", "delete_time"} {
		if counts[name] != p.Total {
			t.Errorf("%s %s count %d, calls %d, measures %+v", p.Name, name, counts[name], p.Total, p.Measures)
		}
	}
}

func recordedNames(ps []report.ProtocolResult) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}
