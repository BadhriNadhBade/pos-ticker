package metrics

import (
	"bufio"
	"fmt"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

var sampleRe = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{(?:[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\]|\\.)*"(?:,|\})?)+)?\s+(\S+)$`)

// parseText is a strict-enough parser for the exposition format: every
// non-comment line must be a valid sample and every metric must have TYPE.
func parseText(t *testing.T, body string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	types := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# TYPE ") {
			f := strings.Fields(line)
			types[f[2]] = f[3]
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		m := sampleRe.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("unparseable line %q", line)
		}
		if _, ok := types[m[1]]; !ok {
			t.Fatalf("sample %q has no TYPE", line)
		}
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			t.Fatalf("bad value in %q: %v", line, err)
		}
		out[m[1]+m[2]] = v
	}
	return out
}

func scrape(t *testing.T, m *Metrics) (string, map[string]float64) {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("content-type %q", ct)
	}
	return rec.Body.String(), parseText(t, rec.Body.String())
}

func TestOutputParsesAndHasExpectedSeries(t *testing.T) {
	m := New()
	m.ObserveHTTP("POST", "/contact", 200)
	m.ObserveHTTP("POST", "/contact", 429)
	m.ObserveHTTP("WEIRD", "unmatched", 404)
	m.Message(MsgAccepted)
	m.Message(MsgRateLimitedIP)
	m.Print(PrintSuccess)
	m.QueueDepth.Set(3)
	m.PrinterUp.Set(1)
	_, got := scrape(t, m)

	want := map[string]float64{
		`http_requests_total{handler="/contact",method="POST",status="2xx"}`:   1,
		`http_requests_total{handler="/contact",method="POST",status="4xx"}`:   1,
		`http_requests_total{handler="unmatched",method="OTHER",status="4xx"}`: 1,
		`printer_messages_total{result="accepted"}`:                            1,
		`printer_messages_total{result="rate_limited_ip"}`:                     1,
		`printer_messages_total{result="rate_limited_global"}`:                 0,
		`printer_prints_total{result="success"}`:                               1,
		`printer_prints_total{result="failed"}`:                                0,
		`printer_queue_depth`:                                                  3,
		`printer_connected`:                                                    1,
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s = %v (present=%v), want %v", k, g, ok, v)
		}
	}
	for _, k := range []string{"go_goroutines", "go_memstats_alloc_bytes", "go_memstats_sys_bytes", "process_uptime_seconds"} {
		if got[k] <= 0 {
			t.Errorf("%s = %v, want > 0", k, got[k])
		}
	}
}

func TestLabelEscaping(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("x_total", "help with \\ and\nnewline", "l")
	c.Inc("a\"b\\c\nd")
	var sb strings.Builder
	if _, err := r.WriteTo(&sb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), `x_total{l="a\"b\\c\nd"} 1`) {
		t.Fatalf("bad escaping:\n%s", sb.String())
	}
	parseText(t, sb.String())
}

func TestCardinalityBounded(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("c_total", "h", "path")
	for i := 0; i < MaxSeriesPerVec*3; i++ {
		c.Inc(fmt.Sprintf("/p/%d", i))
	}
	if n := c.Len(); n > MaxSeriesPerVec+1 {
		t.Fatalf("series=%d exceeds bound", n)
	}
	if c.Value("other") != float64(MaxSeriesPerVec*2) {
		t.Fatalf("overflow bucket = %v", c.Value("other"))
	}
}

func TestConcurrentSafe(t *testing.T) {
	m := New()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				m.ObserveHTTP("GET", "/queue", 200)
				m.QueueDepth.Inc()
				m.QueueDepth.Dec()
				if j%100 == 0 {
					var sb strings.Builder
					_, _ = m.WriteTo(&sb)
				}
			}
		}()
	}
	wg.Wait()
	if v := m.HTTPRequests.Value("/queue", "GET", "2xx"); v != 16*500 {
		t.Fatalf("count = %v", v)
	}
	if m.QueueDepth.Value() != 0 {
		t.Fatalf("gauge = %v", m.QueueDepth.Value())
	}
}

func TestNegativeAddIgnoredAndDuplicatePanics(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("a_total", "h", "l")
	c.Add(-5, "x")
	c.Add(2, "x")
	if c.Value("x") != 2 {
		t.Fatal("negative add not ignored")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("expected duplicate panic")
		}
	}()
	r.NewGauge("a_total", "dup")
}
