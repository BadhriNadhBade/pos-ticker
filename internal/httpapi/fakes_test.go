package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
	"github.com/badhrinadhbade/pos-ticker/internal/metrics"
)

// ------------------------------------------------------------------ fakes

type fakeStore struct {
	mu       sync.Mutex
	msgs     []domain.Message
	nextID   int64
	failPing bool
	inserts  []domain.NewMessage
}

func (f *fakeStore) Ping(context.Context) error {
	if f.failPing {
		return errors.New("db down")
	}
	return nil
}

func (f *fakeStore) InsertMessage(_ context.Context, m domain.NewMessage, maxPerIP, perHour int) (domain.InsertResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, x := range f.msgs {
		if x.IP == m.IP {
			n++
		}
	}
	if n >= maxPerIP {
		return domain.InsertResult{RateLimitedIP: true}, nil
	}
	f.nextID++
	row := domain.Message{
		ID: f.nextID, Name: m.Name, Email: m.Email, Body: m.Body, IP: m.IP, UserAgent: m.UserAgent,
		ReceivedAt: fmt.Sprintf("2026-01-01 00:00:%02d", f.nextID), HeadersJSON: m.HeadersJSON, Image: m.Image,
	}
	if m.BrowserTime != "" {
		bt := m.BrowserTime
		row.BrowserTime = &bt
	}
	used := 0
	for _, x := range f.msgs {
		if x.PrintedAt != nil || x.Claimed == 1 {
			used++
		}
	}
	res := domain.InsertResult{ID: row.ID}
	if used < perHour {
		row.Claimed = 1
		c := row
		res.Claimed = &c
	}
	f.msgs = append(f.msgs, row)
	f.inserts = append(f.inserts, m)
	return res, nil
}

func (f *fakeStore) find(id int64) *domain.Message {
	for i := range f.msgs {
		if f.msgs[i].ID == id {
			return &f.msgs[i]
		}
	}
	return nil
}

func (f *fakeStore) ClaimPending(_ context.Context, id int64) (*domain.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.find(id)
	if m == nil || m.PrintedAt != nil || m.Claimed != 0 {
		return nil, domain.ErrNotFound
	}
	m.Claimed = 1
	c := *m
	return &c, nil
}

func (f *fakeStore) ClaimAllPending(context.Context) ([]domain.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Message
	for i := range f.msgs {
		if f.msgs[i].PrintedAt == nil && f.msgs[i].Claimed == 0 {
			f.msgs[i].Claimed = 1
			out = append(out, f.msgs[i])
		}
	}
	return out, nil
}

func (f *fakeStore) Skip(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.find(id)
	if m == nil || m.PrintedAt != nil || m.Claimed != 0 {
		return domain.ErrNotFound
	}
	m.Claimed = 2
	return nil
}

func (f *fakeStore) MarkPrinted(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := f.find(id); m != nil {
		ts := "2026-01-01 01:00:00"
		m.PrintedAt, m.Claimed = &ts, 0
	}
	return nil
}

func (f *fakeStore) ReleaseClaim(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := f.find(id); m != nil {
		m.Claimed = 0
	}
	return nil
}

func (f *fakeStore) ReleaseStaleClaims(context.Context) (int, error) { return 0, nil }

func (f *fakeStore) List(_ context.Context, limit, offset int) ([]domain.MessageSummary, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []domain.MessageSummary
	for i := len(f.msgs) - 1; i >= 0; i-- { // newest first
		m := f.msgs[i]
		hj := m.HeadersJSON
		all = append(all, domain.MessageSummary{
			ID: m.ID, Name: m.Name, Email: m.Email, Message: m.Body, IP: m.IP, ReceivedAt: m.ReceivedAt,
			BrowserTime: m.BrowserTime, PrintedAt: m.PrintedAt, Headers: &hj, Claimed: m.Claimed, HasImage: len(m.Image) > 0,
		})
	}
	total := len(all)
	if offset > len(all) {
		offset = len(all)
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end], total, nil
}

func (f *fakeStore) Image(_ context.Context, id int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := f.find(id); m != nil && len(m.Image) > 0 {
		return m.Image, nil
	}
	return nil, domain.ErrNotFound
}

func (f *fakeStore) Counts(context.Context) (domain.QueueCounts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var c domain.QueueCounts
	c.Total = len(f.msgs)
	for _, m := range f.msgs {
		if m.PrintedAt == nil && m.Claimed == 0 {
			c.Pending++
		}
		if m.PrintedAt != nil {
			c.PrintedThisHour++
		}
	}
	return c, nil
}

func (f *fakeStore) PrintedTotal(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.msgs {
		if m.PrintedAt != nil {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) LoadSettings(context.Context) (map[string]string, error) { return nil, nil }
func (f *fakeStore) SaveSettings(context.Context, map[string]string) error   { return nil }
func (f *fakeStore) Close() error                                            { return nil }

type fakeSettings struct {
	mu sync.Mutex
	m  map[string]string
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{m: map[string]string{
		"prints_per_hour": "10", "max_per_ip_hour": "3", "line_width": "32",
		"allowed_origin": "https://example.com", "email_notifications": "true",
	}}
}

func (s *fakeSettings) Get(k string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[k]
}

func (s *fakeSettings) Int(k string, def int) int {
	var n int
	if _, err := fmt.Sscanf(s.Get(k), "%d", &n); err != nil {
		return def
	}
	return n
}

func (s *fakeSettings) All() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for k, v := range s.m {
		out[k] = v
	}
	return out
}

func (s *fakeSettings) Patch(_ context.Context, kv map[string]string) (map[string]string, error) {
	s.mu.Lock()
	for k := range kv {
		if _, ok := s.m[k]; !ok {
			s.mu.Unlock()
			return nil, fmt.Errorf("Unknown settings: [%s]", k)
		}
	}
	for k, v := range kv {
		s.m[k] = v
	}
	s.mu.Unlock()
	return s.All(), nil
}

type fakeQueue struct {
	mu      sync.Mutex
	jobs    []domain.Job
	err     error
	depth   int
	up      bool
	mode    string
	panicOn bool
}

func (q *fakeQueue) Submit(j domain.Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return q.err
	}
	q.jobs = append(q.jobs, j)
	return nil
}

func (q *fakeQueue) Depth() int {
	if q.panicOn {
		panic("boom: secret-internal-detail")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.depth + len(q.jobs)
}

func (q *fakeQueue) Status() domain.PrinterStatus {
	if q.mode != "" {
		return domain.PrinterStatus{Up: q.up, Mode: q.mode}
	}
	return domain.PrinterStatus{Up: q.up, Mode: "network:1.2.3.4:9100"}
}

func (q *fakeQueue) snapshot() []domain.Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]domain.Job(nil), q.jobs...)
}

var pngMagic = []byte("\x89PNG\r\n\x1a\n")

type fakeRenderer struct{}

func (fakeRenderer) PrepareImage(raw []byte) (*domain.Bitmap, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty image")
	}
	if !bytes.HasPrefix(raw, pngMagic) {
		return nil, errors.New("unreadable image")
	}
	return &domain.Bitmap{W: 2, H: 2, Pix: []bool{true, false, false, true}}, nil
}
func (fakeRenderer) ASCII(b *domain.Bitmap, cols int) string {
	return fmt.Sprintf("ascii-%dx%d-%d", b.W, b.H, cols)
}
func (fakeRenderer) PreviewReceipt(m domain.Message, _ domain.Settings) ([]byte, error) {
	return tinyPNG(), nil
}
func (fakeRenderer) PreviewBitmap(*domain.Bitmap) ([]byte, error) { return tinyPNG(), nil }

func tinyPNG() []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, image.NewGray(image.Rect(0, 0, 2, 2)))
	return buf.Bytes()
}

// validImage returns bytes accepted by fakeRenderer plus their base64.
func validImage() ([]byte, string) {
	raw := append(append([]byte{}, pngMagic...), []byte("payload-original-bytes")...)
	return raw, base64.StdEncoding.EncodeToString(raw)
}

type fakeMailer struct{ ch chan [5]string }

func (m *fakeMailer) Notify(name, email, message, ip, ts string) {
	m.ch <- [5]string{name, email, message, ip, ts}
}

type fakeStats struct{ ok, failed uint64 }

func (s fakeStats) PrintTotals() (uint64, uint64) { return s.ok, s.failed }

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// -------------------------------------------------------------------- env

const testKey = "0123456789abcdef-test-key"

type env struct {
	t        *testing.T
	store    *fakeStore
	settings *fakeSettings
	queue    *fakeQueue
	mailer   *fakeMailer
	clock    *fakeClock
	deps     Deps
	admin    http.Handler
	public   http.Handler
}

func newEnv(t *testing.T, mutate ...func(*Deps)) *env {
	t.Helper()
	e := &env{
		t: t, store: &fakeStore{}, settings: newFakeSettings(), queue: &fakeQueue{up: true},
		mailer: &fakeMailer{ch: make(chan [5]string, 4)},
		clock:  &fakeClock{t: time.Date(2026, 5, 12, 21, 41, 0, 0, time.UTC)},
	}
	e.deps = Deps{
		Store: e.store, Settings: e.settings, Queue: e.queue, Renderer: fakeRenderer{},
		Mailer: e.mailer, Now: e.clock.Now, Hub: NewHub(), Metrics: metrics.New(),
		UI: fstest.MapFS{
			"index.html": {Data: []byte("<!doctype html><title>x</title>")},
			"app.js":     {Data: []byte("console.log(1)")},
			"app.css":    {Data: []byte("body{}")},
		},
		Config: Config{AdminKey: testKey, DisplayTZ: time.UTC},
	}
	for _, m := range mutate {
		m(&e.deps)
	}
	var err error
	if e.public, err = NewPublicHandler(e.deps); err != nil {
		t.Fatal(err)
	}
	if e.admin, err = NewAdminHandler(e.deps); err != nil {
		t.Fatal(err)
	}
	return e
}

type reqOpt func(*http.Request)

func hdr(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }
func remote(addr string) reqOpt {
	return func(r *http.Request) { r.RemoteAddr = addr }
}
func withKey() reqOpt { return hdr("X-Admin-Key", testKey) }

func do(h http.Handler, method, path, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = "203.0.113.7:4000"
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func login(t *testing.T, e *env) *http.Cookie {
	t.Helper()
	w := do(e.admin, "POST", "/login", `{"key":"`+testKey+`"}`)
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func withCookie(c *http.Cookie) reqOpt {
	return func(r *http.Request) { r.AddCookie(c) }
}
