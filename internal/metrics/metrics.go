// Package metrics is a tiny, dependency-free implementation of the Prometheus
// text exposition format (version 0.0.4): labelled counters, gauges,
// callback-backed gauges/counters and a handler. Everything is safe for
// concurrent use and label cardinality is bounded per metric.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MaxSeriesPerVec bounds the number of distinct label combinations a
// CounterVec will track. Further combinations are folded into an
// "other" series so memory can never grow without bound.
const MaxSeriesPerVec = 512

const overflowLabel = "other"

type collector interface {
	name() string
	write(w *bufio.Writer)
}

// Registry holds collectors and renders them in the text format.
type Registry struct {
	mu         sync.RWMutex
	collectors []collector
	names      map[string]struct{}
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{names: map[string]struct{}{}}
}

func (r *Registry) register(c collector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.names[c.name()]; dup {
		panic("metrics: duplicate metric " + c.name())
	}
	r.names[c.name()] = struct{}{}
	r.collectors = append(r.collectors, c)
}

// WriteTo renders every collector. Output order is registration order, series
// within a metric are sorted, so the output is deterministic.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	cw := &countingWriter{w: w}
	bw := bufio.NewWriter(cw)
	r.mu.RLock()
	cs := append([]collector(nil), r.collectors...)
	r.mu.RUnlock()
	for _, c := range cs {
		c.write(bw)
	}
	err := bw.Flush()
	return cw.n, err
}

// Handler serves the registry in Prometheus text format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = r.WriteTo(w)
	})
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// ---------------------------------------------------------------- CounterVec

type series struct {
	vals []string
	v    float64
}

// CounterVec is a counter with a fixed set of label names.
type CounterVec struct {
	nm, help string
	labels   []string
	mu       sync.Mutex
	series   map[string]*series
}

// NewCounterVec registers and returns a labelled counter. Name should end in _total.
func (r *Registry) NewCounterVec(name, help string, labels ...string) *CounterVec {
	c := &CounterVec{nm: name, help: help, labels: labels, series: map[string]*series{}}
	r.register(c)
	return c
}

func (c *CounterVec) name() string { return c.nm }

func seriesKey(vals []string) string { return strings.Join(vals, "\xff") }

func (c *CounterVec) get(vals []string) *series {
	if len(vals) != len(c.labels) {
		panic(fmt.Sprintf("metrics: %s expects %d label values, got %d", c.nm, len(c.labels), len(vals)))
	}
	k := seriesKey(vals)
	if s, ok := c.series[k]; ok {
		return s
	}
	if len(c.series) >= MaxSeriesPerVec {
		o := make([]string, len(vals))
		for i := range o {
			o[i] = overflowLabel
		}
		k = seriesKey(o)
		if s, ok := c.series[k]; ok {
			return s
		}
		vals = o
	}
	s := &series{vals: append([]string(nil), vals...)}
	c.series[k] = s
	return s
}

// Init makes a series exist (at zero) so it is exported before first use.
func (c *CounterVec) Init(vals ...string) {
	c.mu.Lock()
	c.get(vals)
	c.mu.Unlock()
}

// Add adds delta (ignored if negative or NaN) to the series.
func (c *CounterVec) Add(delta float64, vals ...string) {
	if !(delta >= 0) {
		return
	}
	c.mu.Lock()
	c.get(vals).v += delta
	c.mu.Unlock()
}

// Inc adds one.
func (c *CounterVec) Inc(vals ...string) { c.Add(1, vals...) }

// Set overwrites the value. Intended for mirroring a counter that is owned by
// another component (e.g. printer totals read at scrape time); the value must
// be monotonic for Prometheus semantics.
func (c *CounterVec) Set(v float64, vals ...string) {
	c.mu.Lock()
	c.get(vals).v = v
	c.mu.Unlock()
}

// Value returns the current value of a series (0 if absent). Mostly for tests.
func (c *CounterVec) Value(vals ...string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.series[seriesKey(vals)]; ok {
		return s.v
	}
	return 0
}

// Len returns the number of series.
func (c *CounterVec) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.series)
}

func (c *CounterVec) write(w *bufio.Writer) {
	c.mu.Lock()
	ss := make([]series, 0, len(c.series))
	for _, s := range c.series {
		ss = append(ss, series{vals: s.vals, v: s.v})
	}
	c.mu.Unlock()
	sort.Slice(ss, func(i, j int) bool { return seriesKey(ss[i].vals) < seriesKey(ss[j].vals) })
	writeHeader(w, c.nm, c.help, "counter")
	for _, s := range ss {
		w.WriteString(c.nm)
		writeLabels(w, c.labels, s.vals)
		w.WriteByte(' ')
		w.WriteString(formatFloat(s.v))
		w.WriteByte('\n')
	}
}

// --------------------------------------------------------------------- Gauge

// Gauge is a single float64 value.
type Gauge struct {
	nm, help string
	bits     atomic.Uint64
}

// NewGauge registers and returns a gauge.
func (r *Registry) NewGauge(name, help string) *Gauge {
	g := &Gauge{nm: name, help: help}
	r.register(g)
	return g
}

func (g *Gauge) name() string { return g.nm }

// Set stores v.
func (g *Gauge) Set(v float64) { g.bits.Store(math.Float64bits(v)) }

// Add adds delta (may be negative).
func (g *Gauge) Add(delta float64) {
	for {
		old := g.bits.Load()
		if g.bits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+delta)) {
			return
		}
	}
}

// Inc adds one; Dec subtracts one.
func (g *Gauge) Inc() { g.Add(1) }
func (g *Gauge) Dec() { g.Add(-1) }

// Value returns the current value.
func (g *Gauge) Value() float64 { return math.Float64frombits(g.bits.Load()) }

func (g *Gauge) write(w *bufio.Writer) {
	writeHeader(w, g.nm, g.help, "gauge")
	fmt.Fprintf(w, "%s %s\n", g.nm, formatFloat(g.Value()))
}

// ------------------------------------------------------------------ FuncVals

type funcMetric struct {
	nm, help, typ string
	fn            func() float64
}

func (f *funcMetric) name() string { return f.nm }
func (f *funcMetric) write(w *bufio.Writer) {
	writeHeader(w, f.nm, f.help, f.typ)
	fmt.Fprintf(w, "%s %s\n", f.nm, formatFloat(f.fn()))
}

// NewGaugeFunc registers a gauge whose value is computed at scrape time.
func (r *Registry) NewGaugeFunc(name, help string, fn func() float64) {
	r.register(&funcMetric{nm: name, help: help, typ: "gauge", fn: fn})
}

// NewCounterFunc registers a counter whose value is computed at scrape time.
func (r *Registry) NewCounterFunc(name, help string, fn func() float64) {
	r.register(&funcMetric{nm: name, help: help, typ: "counter", fn: fn})
}

// ----------------------------------------------------------- text formatting

func writeHeader(w *bufio.Writer, name, help, typ string) {
	w.WriteString("# HELP ")
	w.WriteString(name)
	w.WriteByte(' ')
	w.WriteString(escapeHelp(help))
	w.WriteString("\n# TYPE ")
	w.WriteString(name)
	w.WriteByte(' ')
	w.WriteString(typ)
	w.WriteByte('\n')
}

func writeLabels(w *bufio.Writer, names, vals []string) {
	if len(names) == 0 {
		return
	}
	w.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			w.WriteByte(',')
		}
		w.WriteString(n)
		w.WriteString(`="`)
		w.WriteString(escapeLabel(vals[i]))
		w.WriteByte('"')
	}
	w.WriteByte('}')
}

var (
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	labelEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
)

func escapeHelp(s string) string  { return helpEscaper.Replace(s) }
func escapeLabel(s string) string { return labelEscaper.Replace(s) }

func formatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// ------------------------------------------------------------ app metric set

// Metrics is the application metric set. Names are compatible with the legacy
// Python server (printer_messages_total, printer_prints_total,
// printer_queue_depth, printer_connected) and its FastAPI instrumentator
// (http_requests_total{handler,method,status}).
type Metrics struct {
	*Registry
	HTTPRequests *CounterVec // http_requests_total{handler,method,status}
	Messages     *CounterVec // printer_messages_total{result}
	Prints       *CounterVec // printer_prints_total{result}
	QueueDepth   *Gauge      // printer_queue_depth
	PrinterUp    *Gauge      // printer_connected
}

// Allowed values for the "result" labels.
const (
	MsgAccepted          = "accepted"
	MsgRateLimitedIP     = "rate_limited_ip"
	MsgRateLimitedGlobal = "rate_limited_global"
	PrintSuccess         = "success"
	PrintFailed          = "failed"
)

var start = time.Now()

// New builds the metric set plus Go runtime collectors.
func New() *Metrics {
	r := NewRegistry()
	m := &Metrics{Registry: r}
	m.HTTPRequests = r.NewCounterVec("http_requests_total", "Total HTTP requests by route template, method and status class", "handler", "method", "status")
	m.Messages = r.NewCounterVec("printer_messages_total", "Contact form submissions", "result")
	m.Prints = r.NewCounterVec("printer_prints_total", "Print attempts", "result")
	m.QueueDepth = r.NewGauge("printer_queue_depth", "Messages in print queue")
	m.PrinterUp = r.NewGauge("printer_connected", "1 = last print succeeded")
	for _, v := range []string{MsgAccepted, MsgRateLimitedIP, MsgRateLimitedGlobal} {
		m.Messages.Init(v)
	}
	for _, v := range []string{PrintSuccess, PrintFailed} {
		m.Prints.Init(v)
	}
	registerRuntime(r)
	return m
}

var knownMethods = map[string]bool{
	"GET": true, "POST": true, "PATCH": true, "PUT": true, "DELETE": true,
	"HEAD": true, "OPTIONS": true,
}

// ObserveHTTP records a finished request. route must be a route template
// (e.g. "/messages/{id}/image"), never a raw URL path.
func (m *Metrics) ObserveHTTP(method, route string, status int) {
	if !knownMethods[method] {
		method = "OTHER"
	}
	m.HTTPRequests.Inc(route, method, statusClass(status))
}

// Message counts a contact-form outcome (see Msg* constants).
func (m *Metrics) Message(result string) { m.Messages.Inc(result) }

// Print counts a print attempt (see Print* constants).
func (m *Metrics) Print(result string) { m.Prints.Inc(result) }

func statusClass(code int) string {
	if code < 100 || code > 599 {
		return "other"
	}
	return strconv.Itoa(code/100) + "xx"
}

// ------------------------------------------------------------------- runtime

type memSnapshot struct {
	mu   sync.Mutex
	at   time.Time
	stat runtime.MemStats
}

// get returns MemStats, cached for one second so a burst of gauge reads in a
// single scrape triggers only one stop-the-world ReadMemStats.
func (m *memSnapshot) get() runtime.MemStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Since(m.at) > time.Second {
		runtime.ReadMemStats(&m.stat)
		m.at = time.Now()
	}
	return m.stat
}

func registerRuntime(r *Registry) {
	ms := &memSnapshot{}
	r.NewGaugeFunc("go_goroutines", "Number of goroutines that currently exist", func() float64 { return float64(runtime.NumGoroutine()) })
	r.NewGaugeFunc("go_memstats_alloc_bytes", "Bytes of allocated heap objects", func() float64 { return float64(ms.get().Alloc) })
	r.NewGaugeFunc("go_memstats_heap_inuse_bytes", "Bytes in in-use heap spans", func() float64 { return float64(ms.get().HeapInuse) })
	r.NewGaugeFunc("go_memstats_sys_bytes", "Bytes obtained from the system", func() float64 { return float64(ms.get().Sys) })
	r.NewCounterFunc("go_memstats_alloc_bytes_total", "Cumulative bytes allocated for heap objects", func() float64 { return float64(ms.get().TotalAlloc) })
	r.NewCounterFunc("go_gc_cycles_total", "Completed GC cycles", func() float64 { return float64(ms.get().NumGC) })
	r.NewGaugeFunc("process_uptime_seconds", "Seconds since the process started", func() float64 { return time.Since(start).Seconds() })
}
