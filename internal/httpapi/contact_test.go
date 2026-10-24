package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
	"github.com/badhrinadhbade/pos-ticker/internal/metrics"
)

func jsonBody(t *testing.T, w interface{ Result() *http.Response }, v any) {
	t.Helper()
	if err := json.NewDecoder(w.Result().Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func contactJSON(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func TestContactHappyPathPipeline(t *testing.T) {
	hub := NewHub()
	e := newEnv(t, func(d *Deps) { d.Hub = hub })
	s := hub.Subscribe()
	defer s.Close()

	raw, b64 := validImage()
	body := contactJSON(map[string]any{
		"name": "  Ada Lovelace ", "email": "ada@example.com", "message": " hello world ",
		"browser_time": "Tue\x00 May 12  2:41 PM", "image_b64": b64,
	})
	w := do(e.public, "POST", "/contact", body, hdr("User-Agent", "UA/1"), hdr("Cookie", "secret=1"), hdr("X-Admin-Key", "zzz"))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var resp map[string]bool
	jsonBody(t, w, &resp)
	if !resp["ok"] || resp["queued"] {
		t.Fatalf("resp = %v (want ok:true queued:false)", resp)
	}

	// Store got the validated, trimmed fields and the ORIGINAL image bytes.
	if len(e.store.inserts) != 1 {
		t.Fatalf("inserts = %d", len(e.store.inserts))
	}
	in := e.store.inserts[0]
	if in.Name != "Ada Lovelace" || in.Body != "hello world" || in.BrowserTime != "Tue May 12  2:41 PM" {
		t.Errorf("fields = %+v", in)
	}
	if string(in.Image) != string(raw) {
		t.Error("stored image is not the original upload")
	}
	if in.IP != "203.0.113.7" || in.UserAgent != "UA/1" {
		t.Errorf("ip/ua = %q %q", in.IP, in.UserAgent)
	}
	// Credentials must be redacted before persistence.
	var hm map[string]string
	if err := json.Unmarshal([]byte(in.HeadersJSON), &hm); err != nil {
		t.Fatal(err)
	}
	if hm["cookie"] != "[redacted]" || hm["x-admin-key"] != "[redacted]" || hm["user-agent"] != "UA/1" {
		t.Errorf("headers = %v", hm)
	}
	if strings.Contains(in.HeadersJSON, "secret=1") || strings.Contains(in.HeadersJSON, "zzz") {
		t.Error("credential leaked into stored headers")
	}

	// Queue got the claimed row.
	jobs := e.queue.snapshot()
	if len(jobs) != 1 || jobs[0].Kind != domain.JobMessage || jobs[0].Msg == nil || jobs[0].MsgID != 1 {
		t.Fatalf("jobs = %+v", jobs)
	}
	// Mailer called asynchronously with browser_time as timestamp.
	select {
	case got := <-e.mailer.ch:
		if got[0] != "Ada Lovelace" || got[3] != "203.0.113.7" || got[4] != "Tue May 12  2:41 PM" {
			t.Errorf("mail = %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mailer not called")
	}
	// Hub saw a message event then a status event.
	var types []string
	for i := 0; i < 2; i++ {
		select {
		case ev := <-s.C:
			types = append(types, ev.Type)
			if ev.Type == "message" {
				sum := ev.Data.(domain.MessageSummary)
				if sum.ID != 1 || !sum.HasImage || sum.Name != "Ada Lovelace" {
					t.Errorf("summary = %+v", sum)
				}
			}
		case <-time.After(time.Second):
			t.Fatal("missing hub events, got " + fmt.Sprint(types))
		}
	}
	if types[0] != "message" || types[1] != "status" {
		t.Errorf("events = %v", types)
	}
}

func TestContactMetricsAndMailerToggle(t *testing.T) {
	m := metrics.New()
	e := newEnv(t, func(d *Deps) { d.Metrics = m })
	e.settings.m["email_notifications"] = "false"
	w := do(e.public, "POST", "/contact", contactJSON(map[string]any{"name": "Ada", "message": "hi"}))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if m.Messages.Value("accepted") != 1 {
		t.Error("accepted counter")
	}
	select {
	case <-e.mailer.ch:
		t.Fatal("mailer called although email_notifications=false")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestContactValidationHTTP(t *testing.T) {
	e := newEnv(t)
	_, b64 := validImage()
	cases := []struct {
		name string
		body string
		code int
		err  string
	}{
		{"empty body", ``, 400, "missing fields"},
		{"not json", `{{{`, 400, "missing fields"},
		{"array json", `[1,2]`, 400, "missing fields"},
		{"non-string fields", `{"name":5,"message":true}`, 400, "missing fields"},
		{"no message", `{"name":"Ada"}`, 400, "missing fields"},
		{"bad email", `{"name":"Ada","message":"x","email":"nope"}`, 400, "invalid email"},
		{"too long", contactJSON(map[string]any{"name": "Ada", "message": strings.Repeat("a", 501)}), 400, "message too long (max 500 characters)"},
		{"long beyond truncation", contactJSON(map[string]any{"name": "Ada", "message": strings.Repeat("a", 5000)}), 400, "message too long (max 500 characters)"},
		{"gibberish name", `{"name":"asdfgh","message":"x"}`, 400, "invalid name"},
		{"digit name", `{"name":"12345","message":"x"}`, 400, "invalid name"},
		{"bad b64", contactJSON(map[string]any{"name": "Ada", "message": "x", "image_b64": "!!!notbase64"}), 400, "invalid image data"},
		{"b64 with newline", contactJSON(map[string]any{"name": "Ada", "message": "x", "image_b64": b64[:8] + "\n" + b64[8:]}), 400, "invalid image data"},
		{"not an image", contactJSON(map[string]any{"name": "Ada", "message": "x", "image_b64": "aGVsbG8="}), 400, "unreadable image"},
		{"image b64 too big", contactJSON(map[string]any{"name": "Ada", "message": "x", "image_b64": strings.Repeat("A", 10*1024*1024*4/3+8)}), 400, "image too large"},
		{"body beyond limit", contactJSON(map[string]any{"name": "Ada", "message": "x", "image_b64": strings.Repeat("A", 10*1024*1024*2)}), 413, "request too large"},
		{"ok 500 runes", contactJSON(map[string]any{"name": "Ada", "message": strings.Repeat("é", 500)}), 200, ""},
	}
	for i, c := range cases {
		// distinct IP per case so the per-IP limit never interferes
		w := do(e.public, "POST", "/contact", c.body, remote(fmt.Sprintf("198.51.100.%d:1", i+1)), hdr("Content-Type", "application/json"))
		if w.Code != c.code {
			t.Errorf("%s: status %d, want %d (%s)", c.name, w.Code, c.code, w.Body)
			continue
		}
		if c.err != "" {
			var r map[string]string
			jsonBody(t, w, &r)
			if r["error"] != c.err {
				t.Errorf("%s: error %q, want %q", c.name, r["error"], c.err)
			}
		}
	}
	// Nothing invalid reached the store.
	if n := len(e.store.inserts); n != 1 {
		t.Errorf("inserts = %d, want 1", n)
	}
}

func TestContactRateLimitPerIP429(t *testing.T) {
	m := metrics.New()
	e := newEnv(t, func(d *Deps) { d.Metrics = m })
	e.settings.m["max_per_ip_hour"] = "2"
	post := func() int {
		return do(e.public, "POST", "/contact", `{"name":"Ada","message":"hi"}`).Code
	}
	if a, b := post(), post(); a != 200 || b != 200 {
		t.Fatalf("first two = %d %d", a, b)
	}
	w := do(e.public, "POST", "/contact", `{"name":"Ada","message":"hi"}`)
	if w.Code != 429 {
		t.Fatalf("third = %d", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"error":"too many requests"}` {
		t.Errorf("body = %s", got)
	}
	if m.Messages.Value("rate_limited_ip") != 1 {
		t.Error("rate_limited_ip counter")
	}
	// A different IP is unaffected.
	if c := do(e.public, "POST", "/contact", `{"name":"Ada","message":"hi"}`, remote("192.0.2.9:1")).Code; c != 200 {
		t.Errorf("other ip = %d", c)
	}
}

func TestContactGlobalLimitLeavesPending(t *testing.T) {
	m := metrics.New()
	e := newEnv(t, func(d *Deps) { d.Metrics = m })
	e.settings.m["prints_per_hour"] = "1"
	if c := do(e.public, "POST", "/contact", `{"name":"Ada","message":"one"}`, remote("192.0.2.1:1")).Code; c != 200 {
		t.Fatal(c)
	}
	w := do(e.public, "POST", "/contact", `{"name":"Bob","message":"two"}`, remote("192.0.2.2:1"))
	var r map[string]bool
	jsonBody(t, w, &r)
	if w.Code != 200 || !r["ok"] || !r["queued"] {
		t.Fatalf("resp = %d %v", w.Code, r)
	}
	if n := len(e.queue.snapshot()); n != 1 {
		t.Errorf("jobs = %d, want only the first", n)
	}
	if m.Messages.Value("rate_limited_global") != 1 || m.Messages.Value("accepted") != 1 {
		t.Error("counters wrong")
	}
}

func TestContactSubmitFailureReleasesClaim(t *testing.T) {
	e := newEnv(t)
	e.queue.err = fmt.Errorf("queue closed")
	w := do(e.public, "POST", "/contact", `{"name":"Ada","message":"hi"}`)
	var r map[string]bool
	jsonBody(t, w, &r)
	if w.Code != 200 || !r["queued"] {
		t.Fatalf("resp = %d %v", w.Code, r)
	}
	if e.store.msgs[0].Claimed != 0 {
		t.Error("claim not released after submit failure")
	}
}

func TestContactMaxBytes(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Config.MaxImageBytes = 1024 })
	big := contactJSON(map[string]any{"name": "Ada", "message": "x", "pad": strings.Repeat("a", 2*1024+64*1024+10)})
	w := do(e.public, "POST", "/contact", big)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
	if len(e.store.inserts) != 0 {
		t.Error("oversized body reached the store")
	}
	// just under the limit still works
	ok := contactJSON(map[string]any{"name": "Ada", "message": "x", "pad": strings.Repeat("a", 60*1024)})
	if w := do(e.public, "POST", "/contact", ok); w.Code != 200 {
		t.Errorf("under-limit status = %d", w.Code)
	}
}

func TestClientIPSelection(t *testing.T) {
	hdrs := []reqOpt{hdr("Cf-Connecting-Ip", "198.51.100.5"), hdr("X-Forwarded-For", "192.0.2.77, 10.0.0.1")}
	loop := remote("127.0.0.1:5555") // the local tunnel/proxy
	for _, c := range []struct {
		trust bool
		hdrs  []reqOpt
		want  string
	}{
		{false, append([]reqOpt{loop}, hdrs...), "127.0.0.1"},
		{false, hdrs, "203.0.113.7"},
		{true, append([]reqOpt{loop}, hdrs...), "198.51.100.5"},                            // Cf-Connecting-Ip wins
		{true, []reqOpt{loop, hdr("X-Forwarded-For", "192.0.2.77, 10.0.0.1")}, "10.0.0.1"}, // rightmost hop
		{true, []reqOpt{loop, addHdr("X-Forwarded-For", "1.1.1.1"), addHdr("X-Forwarded-For", "6.6.6.6, 192.0.2.8")}, "192.0.2.8"},
		{true, []reqOpt{loop, hdr("Cf-Connecting-Ip", "not-an-ip"), hdr("X-Forwarded-For", "2001:db8:1:2:3:4:5:6")}, "2001:db8:1:2::/64"},
		{true, []reqOpt{loop, hdr("X-Forwarded-For", "garbage")}, "127.0.0.1"},
		{true, []reqOpt{loop}, "127.0.0.1"},
		{true, []reqOpt{remote("10.1.2.3:9")}, "10.1.2.3"},
		{true, []reqOpt{remote("10.1.2.3:9"), hdr("Cf-Connecting-Ip", "192.0.2.1")}, "192.0.2.1"},
		// Headers from a public (non-loopback/private) peer are spoofable: ignored.
		{true, hdrs, "203.0.113.7"},
		{true, []reqOpt{remote("[2001:db8::9]:1"), hdr("Cf-Connecting-Ip", "192.0.2.1")}, "2001:db8::/64"},
	} {
		e := newEnv(t, func(d *Deps) { d.Config.TrustProxy = c.trust })
		do(e.public, "POST", "/contact", `{"name":"Ada","message":"hi"}`, c.hdrs...)
		if got := e.store.inserts[0].IP; got != c.want {
			t.Errorf("trust=%v: ip %q, want %q", c.trust, got, c.want)
		}
	}
}

func TestPublicCORSAndPreflight(t *testing.T) {
	e := newEnv(t)
	w := do(e.public, "OPTIONS", "/contact", "", hdr("Origin", "https://example.com"))
	if w.Code != 204 {
		t.Fatalf("preflight = %d", w.Code)
	}
	h := w.Header()
	if h.Get("Access-Control-Allow-Origin") != "https://example.com" ||
		h.Get("Access-Control-Allow-Methods") != "POST, GET, OPTIONS" ||
		h.Get("Access-Control-Allow-Headers") != "Content-Type" {
		t.Errorf("cors headers = %v", h)
	}
	if w := do(e.public, "OPTIONS", "/contact/count", ""); w.Code != 204 {
		t.Errorf("count preflight = %d", w.Code)
	}
	// CORS also on real responses, including errors.
	if w := do(e.public, "POST", "/contact", `{}`); w.Header().Get("Access-Control-Allow-Origin") != "https://example.com" {
		t.Error("CORS missing on error response")
	}
	// Never "*" (and never emitted when misconfigured as "*" or empty).
	for _, bad := range []string{"*", ""} {
		e.settings.m["allowed_origin"] = bad
		w := do(e.public, "POST", "/contact", `{}`)
		if v := w.Header().Get("Access-Control-Allow-Origin"); v != "" {
			t.Errorf("allowed_origin=%q leaked ACAO=%q", bad, v)
		}
	}
	// Preflight for unknown paths is a 404 without CORS.
	if w := do(e.public, "OPTIONS", "/queue", ""); w.Code != 404 || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("OPTIONS /queue = %d", w.Code)
	}
}

func TestContactCount(t *testing.T) {
	e := newEnv(t)
	ts := "2026-01-01 00:00:00"
	e.store.msgs = []domain.Message{{ID: 1, PrintedAt: &ts}, {ID: 2}, {ID: 3, PrintedAt: &ts}}
	w := do(e.public, "GET", "/contact/count", "")
	if got := strings.TrimSpace(w.Body.String()); w.Code != 200 || got != `{"printed":2}` {
		t.Errorf("%d %s", w.Code, got)
	}
}

func TestPublicListener404ForEverythingElse(t *testing.T) {
	e := newEnv(t)
	for _, p := range []struct{ method, path string }{
		{"GET", "/"}, {"GET", "/admin"}, {"GET", "/health"}, {"GET", "/metrics"}, {"GET", "/queue"},
		{"GET", "/messages"}, {"GET", "/messages/1/image"}, {"GET", "/settings"}, {"PATCH", "/settings"},
		{"POST", "/drain"}, {"POST", "/print/1"}, {"POST", "/print/image"}, {"POST", "/skip/1"},
		{"POST", "/login"}, {"GET", "/events"}, {"POST", "/preview"}, {"GET", "/assets/app.js"},
		{"GET", "/docs"}, {"GET", "/openapi.json"}, {"GET", "/contact"}, {"DELETE", "/contact"},
		{"PUT", "/contact/count"}, {"POST", "/contact/count"}, {"GET", "/contact/"}, {"GET", "/contact/../admin"}, {"POST", "//contact"}, {"POST", "/contact/./"},
	} {
		// even with valid admin credentials the public listener must not serve these
		w := do(e.public, p.method, p.path, "", withKey())
		if w.Code != 404 {
			t.Errorf("%s %s = %d, want 404", p.method, p.path, w.Code)
		}
	}
}

func TestPublicDoesNotRejectCFHeaders(t *testing.T) {
	e := newEnv(t)
	w := do(e.public, "POST", "/contact", `{"name":"Ada","message":"hi"}`, hdr("CF-Ray", "abc-SJC"), hdr("Cf-Connecting-Ip", "198.51.100.1"))
	if w.Code != 200 {
		t.Errorf("tunnelled contact = %d", w.Code)
	}
}

func TestConstructorValidation(t *testing.T) {
	e := newEnv(t)
	d := e.deps
	d.Config.AdminKey = "short"
	if _, err := NewAdminHandler(d); err == nil {
		t.Error("short admin key accepted")
	}
	d = e.deps
	d.Store = nil
	if _, err := NewPublicHandler(d); err == nil {
		t.Error("nil store accepted")
	}
	// public handler does not need an admin key
	d = e.deps
	d.Config.AdminKey = ""
	if _, err := NewPublicHandler(d); err != nil {
		t.Errorf("public handler: %v", err)
	}
}
