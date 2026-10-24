package httpapi

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
)

func addHdr(k, v string) reqOpt { return func(r *http.Request) { r.Header.Add(k, v) } }
func setHost(h string) reqOpt   { return func(r *http.Request) { r.Host = h } }

const okContact = `{"name":"Ada","message":"hi"}`

// ------------------------------------------------ finding 1: /contact DoS

func TestContactJunkBodyDoesNotBalloon(t *testing.T) {
	e := newEnv(t)
	// ~4 MiB array of numbers under an unknown key. Decoded into
	// map[string]any this would materialise millions of interface values.
	junk := `{"name":"Ada","message":"hi","zzz":[` + strings.Repeat("1,", 2<<20) + `1]}`
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	w := do(e.public, "POST", "/contact", junk)
	runtime.ReadMemStats(&after)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 40<<20 {
		t.Errorf("decoding a %d byte junk body allocated %d MiB", len(junk), alloc>>20)
	}
	if got := e.store.inserts[0]; got.Name != "Ada" || got.Body != "hi" {
		t.Errorf("known fields lost: %+v", got)
	}
	// wrongly typed known fields are ignored, the rest still decodes
	w = do(e.public, "POST", "/contact", `{"name":5,"message":"hi","email":["x"]}`, remote("198.51.100.1:1"))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "missing fields") {
		t.Errorf("typed mismatch: %d %s", w.Code, w.Body)
	}
}

type countingBody struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}
func (c *countingBody) Close() error { return nil }

func TestContactPerIPPrecheckBeforeDecode(t *testing.T) {
	e := newEnv(t)
	e.settings.m["max_per_ip_hour"] = "1"
	if w := do(e.public, "POST", "/contact", okContact); w.Code != 200 {
		t.Fatalf("first = %d", w.Code)
	}
	body := &countingBody{r: strings.NewReader(`{"name":"Ada","message":"` + strings.Repeat("x", 4096) + `"}`)}
	r := httptest.NewRequest("POST", "/contact", body)
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "203.0.113.7:4000"
	w := httptest.NewRecorder()
	e.public.ServeHTTP(w, r)
	if w.Code != 429 || strings.TrimSpace(w.Body.String()) != `{"error":"too many requests"}` {
		t.Fatalf("second = %d %s", w.Code, w.Body)
	}
	if n := body.n.Load(); n != 0 {
		t.Errorf("body was read (%d bytes) before the per-IP check", n)
	}
	if len(e.store.inserts) != 1 {
		t.Errorf("store inserts = %d", len(e.store.inserts))
	}
	// other IPs are unaffected
	if w := do(e.public, "POST", "/contact", okContact, remote("198.51.100.4:1")); w.Code != 200 {
		t.Errorf("other ip = %d", w.Code)
	}
}

func TestContactImageAttemptsLimited(t *testing.T) {
	e := newEnv(t)
	bad := contactJSON(map[string]any{"name": "Ada", "message": "x", "image_b64": "aGVsbG8="})
	codes := map[int]int{}
	for i := 0; i < imageAttemptsMin+5; i++ {
		codes[do(e.public, "POST", "/contact", bad).Code]++
	}
	if codes[400] != imageAttemptsMin || codes[429] != 5 {
		t.Errorf("codes = %v, want %d x 400 then 5 x 429", codes, imageAttemptsMin)
	}
	// the window slides
	e.clock.Advance(2 * time.Minute)
	if c := do(e.public, "POST", "/contact", bad).Code; c != 400 {
		t.Errorf("after window = %d", c)
	}
}

type blockingRenderer struct {
	fakeRenderer
	started chan struct{}
	release chan struct{}
}

func (b blockingRenderer) PrepareImage(raw []byte) (*domain.Bitmap, error) {
	b.started <- struct{}{}
	<-b.release
	return b.fakeRenderer.PrepareImage(raw)
}

func TestContactImageConcurrencyCap503(t *testing.T) {
	br := blockingRenderer{started: make(chan struct{}, 8), release: make(chan struct{})}
	e := newEnv(t, func(d *Deps) { d.Renderer = br })
	_, b64 := validImage()
	body := contactJSON(map[string]any{"name": "Ada", "message": "hi", "image_b64": b64})

	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < imageConcurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes <- do(e.public, "POST", "/contact", body, remote(fmt.Sprintf("198.51.100.%d:1", i+10))).Code
		}(i)
	}
	for i := 0; i < imageConcurrency; i++ {
		select {
		case <-br.started:
		case <-time.After(3 * time.Second):
			t.Fatal("renderer calls did not start")
		}
	}
	w := do(e.public, "POST", "/contact", body, remote("198.51.100.99:1"))
	if w.Code != 503 || strings.TrimSpace(w.Body.String()) != `{"error":"busy, try again"}` || w.Header().Get("Retry-After") == "" {
		t.Errorf("third concurrent = %d %q retry=%q", w.Code, w.Body, w.Header().Get("Retry-After"))
	}
	// text-only submissions are not throttled by the image semaphore
	if w := do(e.public, "POST", "/contact", okContact, remote("198.51.100.98:1")); w.Code != 200 {
		t.Errorf("text-only while busy = %d", w.Code)
	}
	close(br.release)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 200 {
			t.Errorf("blocked request finished with %d", c)
		}
	}
	// slot released: works again
	if w := do(e.public, "POST", "/contact", body, remote("198.51.100.97:1")); w.Code != 200 {
		t.Errorf("after release = %d %s", w.Code, w.Body)
	}
}

func TestContactBodyLimitIsBase64Sized(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Config.MaxImageBytes = 30000 })
	if got, want := e.deps.Config.withDefaults().bodyLimit(), int64(30000*4/3+64*1024); got != want {
		t.Fatalf("bodyLimit = %d, want %d", got, want)
	}
	// Between the new (~105 KiB) and the old 2x (~122 KiB) limit => 413 now.
	big := contactJSON(map[string]any{"name": "Ada", "message": "x", "pad": strings.Repeat("a", 112000)})
	if w := do(e.public, "POST", "/contact", big); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

// ------------------------------------------------ finding 2/3: client IP

func TestAdminIgnoresForwardedHeadersForBackoff(t *testing.T) {
	for _, peer := range []string{"203.0.113.9:1", "127.0.0.1:1", "10.0.0.2:1"} {
		e := newEnv(t, func(d *Deps) { d.Config.TrustProxy = true })
		counts := map[int]int{}
		for i := 0; i < 200; i++ {
			w := do(e.admin, "POST", "/login", `{"key":"wrong-wrong-wrong-wrong"}`, remote(peer),
				hdr("X-Forwarded-For", fmt.Sprintf("198.51.%d.%d", i/250, i%250+1)))
			counts[w.Code]++
		}
		free := e.deps.Config.withDefaults().AuthFreeAttempts
		if counts[401] != free+1 || counts[429] != 200-free-1 {
			t.Errorf("peer %s: codes = %v, want %d x 401 then 429s", peer, counts, free+1)
		}
		// Same via X-Admin-Key on an API route; rotating XFF buys nothing.
		w := do(e.admin, "GET", "/queue", "", withKey(), remote(peer), hdr("X-Forwarded-For", "192.0.2.200"))
		if w.Code != 429 {
			t.Errorf("peer %s: correct key from locked socket with fresh XFF = %d, want 429", peer, w.Code)
		}
		// A different socket address is unaffected.
		w = do(e.admin, "GET", "/queue", "", withKey(), remote("192.0.2.222:1"))
		if w.Code != 200 {
			t.Errorf("other socket = %d", w.Code)
		}
	}
}

func TestAuthTryKeyIsAtomic(t *testing.T) {
	e := newEnv(t)
	a := newAuthenticator(e.deps.Config.withDefaults(), e.clock.Now)
	var evaluated atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, wait := a.tryKey("198.51.100.1", "definitely-wrong-key"); wait == 0 {
				evaluated.Add(1)
			}
		}()
	}
	wg.Wait()
	if free := int64(a.free); evaluated.Load() != free+1 {
		t.Errorf("%d guesses evaluated concurrently, want exactly %d", evaluated.Load(), free+1)
	}
}

func TestIPv6KeyedOnSlash64(t *testing.T) {
	e := newEnv(t)
	e.settings.m["max_per_ip_hour"] = "1"
	if w := do(e.public, "POST", "/contact", okContact, remote("[2001:db8:1:2::1]:1")); w.Code != 200 {
		t.Fatal(w.Code)
	}
	// another address in the same /64 shares the budget (pre-check, no insert)
	if w := do(e.public, "POST", "/contact", okContact, remote("[2001:db8:1:2:abcd:ef01:2345:6789]:1")); w.Code != 429 {
		t.Errorf("same /64 = %d, want 429", w.Code)
	}
	if w := do(e.public, "POST", "/contact", okContact, remote("[2001:db8:1:3::1]:1")); w.Code != 200 {
		t.Errorf("different /64 = %d", w.Code)
	}
	if got := e.store.inserts[0].IP; got != "2001:db8:1:2::/64" {
		t.Errorf("stored ip = %q", got)
	}
	// the authoritative store check sees the same key even if the in-memory
	// counter is cold (e.g. after a restart)
	e2 := newEnv(t)
	e2.settings.m["max_per_ip_hour"] = "1"
	do(e2.public, "POST", "/contact", okContact, remote("[2001:db8:1:2::1]:1"))
	pub, _ := NewPublicHandler(e2.deps) // fresh counters
	if w := do(pub, "POST", "/contact", okContact, remote("[2001:db8:1:2::77]:1")); w.Code != 429 {
		t.Errorf("store-level /64 limit = %d, want 429", w.Code)
	}
	// IPv4-mapped IPv6 is a plain IPv4 key
	if got := ipKey(peerIP(&http.Request{RemoteAddr: "[::ffff:192.0.2.5]:1"})); got != "192.0.2.5" {
		t.Errorf("mapped = %q", got)
	}
}

// ------------------------------------------------ finding 4: CSRF / origin

func TestContactRequiresJSONContentType(t *testing.T) {
	e := newEnv(t)
	for i, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", "application/jsonx", "text/json"} {
		w := do(e.public, "POST", "/contact", okContact, hdr("Content-Type", ct), remote(fmt.Sprintf("198.51.100.%d:1", i+1)))
		if w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("content-type %q = %d, want 415", ct, w.Code)
		}
	}
	if len(e.store.inserts) != 0 {
		t.Error("non-JSON request reached the store")
	}
	for i, ct := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON"} {
		if w := do(e.public, "POST", "/contact", okContact, hdr("Content-Type", ct), remote(fmt.Sprintf("192.0.2.%d:1", i+1))); w.Code != 200 {
			t.Errorf("content-type %q = %d, want 200", ct, w.Code)
		}
	}
	// the 415 is still readable cross-origin by the allowed site
	w := do(e.public, "POST", "/contact", okContact, hdr("Content-Type", "text/plain"), remote("192.0.2.99:1"))
	if w.Header().Get("Access-Control-Allow-Origin") != "https://example.com" {
		t.Error("CORS header missing on 415")
	}
}

func TestContactOriginEnforcedWhenConfigured(t *testing.T) {
	e := newEnv(t) // allowed_origin = https://example.com
	n := 0
	post := func(opts ...reqOpt) *httptest.ResponseRecorder {
		n++
		return do(e.public, "POST", "/contact", okContact, append(opts, remote(fmt.Sprintf("198.51.100.%d:1", n)))...)
	}
	if w := post(hdr("Origin", "https://evil.example")); w.Code != 403 {
		t.Errorf("foreign origin = %d, want 403", w.Code)
	}
	if w := post(hdr("Origin", "null")); w.Code != 403 {
		t.Errorf("null origin = %d, want 403", w.Code)
	}
	if w := post(hdr("Origin", "https://example.com.evil.example")); w.Code != 403 {
		t.Errorf("suffix origin = %d, want 403", w.Code)
	}
	if len(e.store.inserts) != 0 {
		t.Error("forbidden origin reached the store")
	}
	if w := post(hdr("Origin", "https://example.com")); w.Code != 200 {
		t.Errorf("matching origin = %d", w.Code)
	}
	if w := post(); w.Code != 200 { // no Origin header (curl, server-side)
		t.Errorf("no origin = %d", w.Code)
	}
	for _, v := range []string{"", "*"} { // not configured => no enforcement
		e.settings.m["allowed_origin"] = v
		if w := post(hdr("Origin", "https://anything.example")); w.Code != 200 {
			t.Errorf("allowed_origin=%q: %d", v, w.Code)
		}
	}
}

// ------------------------------------------------ finding 5: SSE lifecycle

// openSSE opens /events against a real server and returns a channel that is
// closed when the stream ends, plus a counter of received frames.
func openSSE(t *testing.T, url string, opts ...reqOpt) (ended <-chan struct{}, frames *atomic.Int64, closeFn func()) {
	t.Helper()
	req, _ := http.NewRequest("GET", url+"/events", nil)
	for _, o := range opts {
		// reqOpt works on *http.Request
		o(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("SSE status %d", resp.StatusCode)
	}
	done := make(chan struct{})
	n := &atomic.Int64{}
	got := make(chan struct{}, 1)
	go func() {
		defer close(done)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "event: status") {
				n.Add(1)
				select {
				case got <- struct{}{}:
				default:
				}
			}
		}
	}()
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("no initial status frame")
	}
	return done, n, func() { resp.Body.Close() }
}

func expectEnds(t *testing.T, ended <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatalf("stream did not end: %s", what)
	}
}

func TestSSEClosesOnLogout(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Config.StatusInterval = time.Hour }) // only logout can end it
	srv := httptest.NewServer(e.admin)
	defer srv.Close()
	c := login(t, e)
	ended, _, closeFn := openSSE(t, srv.URL, withCookie(c))
	defer closeFn()
	select {
	case <-ended:
		t.Fatal("stream ended prematurely")
	case <-time.After(100 * time.Millisecond):
	}
	if w := do(e.admin, "POST", "/logout", "", withCookie(c), hdr(csrfHeader, csrfValue)); w.Code != 200 {
		t.Fatalf("logout = %d", w.Code)
	}
	expectEnds(t, ended, "logout")
	// the old cookie no longer opens a stream
	if w := do(e.admin, "GET", "/events", "", withCookie(c)); w.Code != 401 {
		t.Errorf("revoked cookie /events = %d", w.Code)
	}
}

func TestSSEClosesOnSessionExpiry(t *testing.T) {
	e := newEnv(t, func(d *Deps) {
		d.Config.StatusInterval = 20 * time.Millisecond
		d.Config.SessionTTL = time.Hour
	})
	srv := httptest.NewServer(e.admin)
	defer srv.Close()
	c := login(t, e)
	ended, _, closeFn := openSSE(t, srv.URL, withCookie(c))
	defer closeFn()
	// key-auth stream on the same server must survive the clock jump
	keyEnded, keyFrames, keyClose := openSSE(t, srv.URL, withKey())
	defer keyClose()

	e.clock.Advance(2 * time.Hour)
	expectEnds(t, ended, "session expiry")

	before := keyFrames.Load()
	deadline := time.Now().Add(3 * time.Second)
	for keyFrames.Load() <= before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if keyFrames.Load() <= before {
		t.Error("key-authenticated stream stopped receiving")
	}
	select {
	case <-keyEnded:
		t.Error("key-authenticated stream was closed")
	default:
	}
}

func TestSSELifetimeCapped(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Config.SSEMaxLifetime = 150 * time.Millisecond })
	srv := httptest.NewServer(e.admin)
	defer srv.Close()
	ended, _, closeFn := openSSE(t, srv.URL, withKey())
	defer closeFn()
	expectEnds(t, ended, "lifetime cap")
	if got := (Config{}).withDefaults().SSEMaxLifetime; got != time.Hour {
		t.Errorf("default SSE lifetime = %v", got)
	}
}

// ------------------------------------------------ finding 6: misc

type countingStore struct {
	*fakeStore
	calls atomic.Int32
}

func (c *countingStore) PrintedTotal(ctx context.Context) (int, error) {
	c.calls.Add(1)
	return c.fakeStore.PrintedTotal(ctx)
}

func TestContactCountCached(t *testing.T) {
	var cs *countingStore
	e := newEnv(t, func(d *Deps) {
		cs = &countingStore{fakeStore: d.Store.(*fakeStore)}
		d.Store = cs
	})
	ts := "2026-01-01 00:00:00"
	e.store.msgs = []domain.Message{{ID: 1, PrintedAt: &ts}}
	for i := 0; i < 20; i++ {
		if w := do(e.public, "GET", "/contact/count", ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"printed":1}` {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
	if n := cs.calls.Load(); n != 1 {
		t.Errorf("store queried %d times for 20 requests", n)
	}
	e.store.msgs = append(e.store.msgs, domain.Message{ID: 2, PrintedAt: &ts})
	e.clock.Advance(4 * time.Second)
	if w := do(e.public, "GET", "/contact/count", ""); strings.TrimSpace(w.Body.String()) != `{"printed":1}` {
		t.Errorf("stale value expected within TTL, got %s", w.Body)
	}
	e.clock.Advance(2 * time.Second)
	if w := do(e.public, "GET", "/contact/count", ""); strings.TrimSpace(w.Body.String()) != `{"printed":2}` {
		t.Errorf("fresh value expected after TTL, got %s", w.Body)
	}
	if n := cs.calls.Load(); n != 2 {
		t.Errorf("store queried %d times, want 2", n)
	}
}

func TestHealthHidesPrinterAddress(t *testing.T) {
	for _, c := range []struct {
		mode    string
		up      bool
		printer string
		kind    string
	}{
		{"network:192.168.1.50:9100", true, "up", "network"},
		{"network:192.168.1.50:9100", false, "down", "network"},
		{"usb:/dev/usb/lp0", true, "up", "usb"},
		{"unknown", true, "unknown", "unknown"},
		{"weird", false, "unknown", "unknown"},
	} {
		e := newEnv(t)
		e.queue.mode, e.queue.up = c.mode, c.up
		w := do(e.admin, "GET", "/health", "")
		body := w.Body.String()
		for _, leak := range []string{"192.168", "9100", "/dev", "lp0"} {
			if strings.Contains(body, leak) {
				t.Errorf("mode %q: /health leaks %q: %s", c.mode, leak, body)
			}
		}
		var h map[string]any
		jsonBody(t, w, &h)
		if h["printer"] != c.printer || h["printer_mode"] != c.kind || h["ok"] != true || h["db"] != "ok" {
			t.Errorf("mode %q up=%v: %v", c.mode, c.up, h)
		}
		if _, ok := h["queue_depth"]; !ok {
			t.Error("queue_depth missing")
		}
	}
}

func TestAdminAllowedHosts(t *testing.T) {
	// default: everything accepted
	e := newEnv(t)
	if w := do(e.admin, "GET", "/health", "", setHost("anything.example")); w.Code != 200 {
		t.Errorf("default host policy rejected: %d", w.Code)
	}
	if got := e.deps.Config.AllowedHosts; len(got) != 0 {
		t.Errorf("AllowedHosts default = %v", got)
	}

	e = newEnv(t, func(d *Deps) { d.Config.AllowedHosts = []string{"Admin.Lan", "pos.example.com:8000"} })
	for host, want := range map[string]int{
		"admin.lan":         200,
		"ADMIN.LAN:8000":    200,
		"pos.example.com":   200,
		"localhost":         200,
		"localhost:8000":    200,
		"127.0.0.1:8000":    200,
		"192.168.1.20":      200,
		"[::1]:8000":        200,
		"[2001:db8::5]":     200,
		"evil.example":      403,
		"admin.lan.evil.io": 403,
		"evil.example:8000": 403,
		"":                  403,
	} {
		w := do(e.admin, "GET", "/health", "", setHost(host))
		if w.Code != want {
			t.Errorf("Host %q = %d, want %d", host, w.Code, want)
		}
		if want == 403 && w.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("Host %q: rejection lacks security headers", host)
		}
	}
	// the public handler is unaffected (it is behind the tunnel)
	if w := do(e.public, "POST", "/contact", okContact, setHost("evil.example")); w.Code != 200 {
		t.Errorf("public host = %d", w.Code)
	}
}
