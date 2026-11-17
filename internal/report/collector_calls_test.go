package report

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestRecordCall_CountsOrderBytesAndPercentiles(t *testing.T) {
	c := NewCollector()
	// Two operations with very different times. The protocol percentile is
	// computed from every call, not from the two operation percentiles.
	for i := 0; i < 10; i++ {
		c.RecordCall("postgres", "query", time.Millisecond, nil, 10, 20)
	}
	for i := 0; i < 10; i++ {
		c.RecordCall("postgres", "exec", 100*time.Millisecond, errIf(i == 0), 1, 2)
	}
	c.RecordCall("ftp", "download", 5*time.Millisecond, nil, 100, 200)

	res := c.Finish("fixed-vus", 2*time.Second)
	if len(res.Protocols) != 2 || res.Protocols[0].Name != "postgres" || res.Protocols[1].Name != "ftp" {
		t.Fatalf("order = %+v", namesOf(res.Protocols))
	}
	p, ok := res.Protocol("postgres")
	if !ok {
		t.Fatal("missing postgres")
	}
	if p.Total != 20 || p.Failed != 1 || p.ErrorRate != 0.05 {
		t.Fatalf("totals = total %d failed %d rate %v", p.Total, p.Failed, p.ErrorRate)
	}
	if p.BytesSent != 110 || p.BytesReceived != 220 {
		t.Fatalf("bytes sent %d received %d", p.BytesSent, p.BytesReceived)
	}
	if p.BytesPerSec != float64(330)/2 {
		t.Fatalf("bytes/s = %v", p.BytesPerSec)
	}
	// 10 samples at 1ms then 10 at 100ms. Nearest rank: index (p*20)/100.
	if p.Latency.P50 != 100*time.Millisecond || p.Latency.P95 != 100*time.Millisecond {
		t.Fatalf("protocol latency p50 %s p95 %s", p.Latency.P50, p.Latency.P95)
	}
	if p.Operations[0].Name != "query" || p.Operations[0].Latency.P50 != time.Millisecond {
		t.Fatalf("query op = %+v", p.Operations[0])
	}
	if p.Operations[1].Name != "exec" || p.Operations[1].Latency.P95 != 100*time.Millisecond {
		t.Fatalf("exec op = %+v", p.Operations[1])
	}
}

func TestRecordCall_CapAndEmptyAndMeasures(t *testing.T) {
	c := NewCollector()
	for i := 0; i < 34; i++ {
		c.RecordCall("p"+itoa(i), "download", time.Millisecond, nil, 1, 1)
	}
	res := c.Finish("fixed-vus", time.Second)
	if _, ok := res.Protocol("p0"); !ok {
		t.Fatal("first protocol was dropped")
	}
	if _, ok := res.Protocol("p31"); !ok {
		t.Fatal("32nd protocol was dropped")
	}
	if _, ok := res.Protocol("p32"); ok {
		t.Fatal("name past the cap was stored")
	}
	other, ok := res.Protocol("(other)")
	if !ok || other.Total != 2 {
		t.Fatalf("(other) = %+v ok %v", other, ok)
	}

	ops := NewCollector()
	for i := 0; i < 34; i++ {
		ops.RecordCall("ftp", "op"+itoa(i), time.Millisecond, nil, 0, 0)
	}
	got := ops.Finish("fixed-vus", time.Second)
	ftp := got.Protocols[0]
	if len(ftp.Operations) != 33 {
		t.Fatalf("%d operations, want 32 plus (other)", len(ftp.Operations))
	}
	if ftp.Operations[len(ftp.Operations)-1].Name != "(other)" || ftp.Operations[len(ftp.Operations)-1].Total != 2 {
		t.Fatalf("last op = %+v", ftp.Operations[len(ftp.Operations)-1])
	}

	empty := NewCollector().Finish("fixed-vus", time.Second)
	if empty.Protocols != nil {
		t.Fatalf("no calls produced %+v", empty.Protocols)
	}
	raw, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if jsonHasKey(raw, "protocols") {
		t.Fatalf("empty report has protocols: %s", raw)
	}

	m := NewCollector()
	m.RecordCall("ftp", "list", time.Millisecond, nil, 0, 0)
	m.RecordMeasure("ftp", "upload_time", 0)
	m.RecordMeasure("ftp", "connect_time", 3*time.Millisecond)
	out := m.Finish("fixed-vus", time.Second)
	p := out.Protocols[0]
	if p.Total != 1 || len(p.Measures) != 2 || p.Measures[0].Name != "upload_time" || p.Measures[0].Count != 1 {
		t.Fatalf("measures = %+v total %d", p.Measures, p.Total)
	}
	if p.Measures[1].Latency.P50 != 3*time.Millisecond {
		t.Fatalf("connect measure = %+v", p.Measures[1])
	}
}

func TestRecordCall_Concurrent(t *testing.T) {
	c := NewCollector()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				c.RecordCall("redis", "command", time.Millisecond, errIf(n%10 == 0), 3, 4)
			}
		}()
	}
	wg.Wait()
	res := c.Finish("fixed-vus", time.Second)
	p, ok := res.Protocol("redis")
	if !ok || p.Total != 5000 || p.Failed != 500 || p.BytesSent != 15000 || p.BytesReceived != 20000 {
		t.Fatalf("%+v ok %v", p, ok)
	}
}

func namesOf(ps []ProtocolResult) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func jsonHasKey(raw []byte, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
