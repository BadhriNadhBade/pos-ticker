package httpapi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
	"github.com/badhrinadhbade/pos-ticker/internal/metrics"
)

func seed(e *env, n int) {
	for i := 1; i <= n; i++ {
		e.store.msgs = append(e.store.msgs, domain.Message{
			ID: int64(i), Name: fmt.Sprintf("Person%d", i), Email: fmt.Sprintf("p%d@example.com", i),
			Body: fmt.Sprintf("message number %d", i), IP: "1.1.1.1",
			ReceivedAt: fmt.Sprintf("2026-01-01 00:00:%02d", i),
		})
	}
	e.store.nextID = int64(n)
}

// ---------------------------------------------------------------- auth

func TestAuthMatrix(t *testing.T) {
	e := newEnv(t)
	seed(e, 2)
	cookie := login(t, e)

	type tc struct {
		name   string
		method string
		path   string
		body   string
	}
	routes := []tc{
		{"queue", "GET", "/queue", ""},
		{"messages", "GET", "/messages", ""},
		{"image", "GET", "/messages/1/image", ""},
		{"settings", "GET", "/settings", ""},
		{"patch", "PATCH", "/settings", `{"settings":{"line_width":"40"}}`},
		{"drain", "POST", "/drain", ""},
		{"skip", "POST", "/skip/1", ""},
		{"print", "POST", "/print/1", ""},
		{"printimg", "POST", "/print/image", `{"image_b64":"x"}`},
		{"preview", "POST", "/preview", `{"kind":"message","name":"a","message":"b"}`},
		{"metrics", "GET", "/metrics", ""},
	}
	for i, r := range routes {
		// no credentials
		if w := do(e.admin, r.method, r.path, r.body); w.Code != 401 {
			t.Errorf("%s no creds = %d", r.name, w.Code)
		}
		// wrong header key (own IP per route to stay clear of the backoff)
		if w := do(e.admin, r.method, r.path, r.body, hdr("X-Admin-Key", "wrong-key-wrong-key"), remote(fmt.Sprintf("198.51.100.%d:1", i+1))); w.Code != 401 {
			t.Errorf("%s bad key = %d", r.name, w.Code)
		}
		// good header key: never 401/403
		if w := do(e.admin, r.method, r.path, r.body, withKey()); w.Code == 401 || w.Code == 403 {
			t.Errorf("%s good key = %d", r.name, w.Code)
		}
		// forged cookie
		forged := &http.Cookie{Name: cookieName, Value: strings.Replace(cookie.Value, ".", "x.", 1)}
		if w := do(e.admin, r.method, r.path, r.body, withCookie(forged), hdr(csrfHeader, csrfValue)); w.Code != 401 {
			t.Errorf("%s forged cookie = %d", r.name, w.Code)
		}
		// valid cookie + CSRF header
		if w := do(e.admin, r.method, r.path, r.body, withCookie(cookie), hdr(csrfHeader, csrfValue)); w.Code == 401 || w.Code == 403 {
			t.Errorf("%s cookie+csrf = %d", r.name, w.Code)
		}
		// valid cookie but mutating without CSRF header => 403; safe methods exempt
		w := do(e.admin, r.method, r.path, r.body, withCookie(cookie))
		if r.method == "GET" {
			if w.Code == 401 || w.Code == 403 {
				t.Errorf("%s GET cookie = %d", r.name, w.Code)
			}
		} else if w.Code != 403 {
			t.Errorf("%s cookie without CSRF = %d, want 403", r.name, w.Code)
		}
		// header-key auth is exempt from CSRF
		if w := do(e.admin, r.method, r.path, r.body, withKey()); w.Code == 403 {
			t.Errorf("%s key auth demanded CSRF", r.name)
		}
		// wrong CSRF value
		if r.method != "GET" {
			if w := do(e.admin, r.method, r.path, r.body, withCookie(cookie), hdr(csrfHeader, "XMLHttpRequest")); w.Code != 403 {
				t.Errorf("%s wrong CSRF value = %d", r.name, w.Code)
			}
		}
	}
}

func TestPublicEndpointsNeedNoAuth(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/health", "/admin", "/assets/app.js", "/assets/app.css"} {
		if w := do(e.admin, "GET", p, ""); w.Code != 200 {
			t.Errorf("GET %s = %d", p, w.Code)
		}
	}
	w := do(e.admin, "GET", "/", "")
	if w.Code != 302 || w.Header().Get("Location") != "/admin" {
		t.Errorf("/ = %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestBearerOnlyForMetrics(t *testing.T) {
	e := newEnv(t)
	if w := do(e.admin, "GET", "/metrics", "", hdr("Authorization", "Bearer "+testKey)); w.Code != 200 {
		t.Errorf("bearer metrics = %d", w.Code)
	}
	if w := do(e.admin, "GET", "/queue", "", hdr("Authorization", "Bearer "+testKey)); w.Code != 401 {
		t.Errorf("bearer on /queue = %d, want 401", w.Code)
	}
	if w := do(e.admin, "GET", "/metrics", "", hdr("Authorization", "Bearer nope-nope-nope-nope"), remote("198.51.100.9:1")); w.Code != 401 {
		t.Errorf("bad bearer = %d", w.Code)
	}
}

func TestLoginCookieAttributesAndLogout(t *testing.T) {
	e := newEnv(t)
	w := do(e.admin, "POST", "/login", `{"key":"`+testKey+`"}`)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	c := w.Result().Cookies()[0]
	if c.Name != cookieName || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.MaxAge != int((12*time.Hour).Seconds()) {
		t.Errorf("cookie = %+v", c)
	}
	if c.Secure {
		t.Error("Secure set on plain HTTP")
	}
	// Secure over TLS-terminating trusted proxy only
	e2 := newEnv(t, func(d *Deps) { d.Config.TrustProxy = true })
	w2 := do(e2.admin, "POST", "/login", `{"key":"`+testKey+`"}`, hdr("X-Forwarded-Proto", "https"))
	if !w2.Result().Cookies()[0].Secure {
		t.Error("Secure missing behind trusted https proxy")
	}
	w3 := do(e.admin, "POST", "/login", `{"key":"`+testKey+`"}`, hdr("X-Forwarded-Proto", "https"))
	if w3.Result().Cookies()[0].Secure {
		t.Error("X-Forwarded-Proto honoured without TRUST_PROXY")
	}

	if w := do(e.admin, "GET", "/queue", "", withCookie(c)); w.Code != 200 {
		t.Fatalf("session unusable: %d", w.Code)
	}
	// logout without CSRF header is refused; with it the session dies.
	if w := do(e.admin, "POST", "/logout", "", withCookie(c)); w.Code != 403 {
		t.Errorf("logout w/o csrf = %d", w.Code)
	}
	if w := do(e.admin, "POST", "/logout", "", withCookie(c), hdr(csrfHeader, csrfValue)); w.Code != 200 {
		t.Errorf("logout = %d", w.Code)
	}
	if w := do(e.admin, "GET", "/queue", "", withCookie(c)); w.Code != 401 {
		t.Errorf("revoked session still valid: %d", w.Code)
	}
}

func TestSessionExpiry(t *testing.T) {
	e := newEnv(t)
	c := login(t, e)
	e.clock.Advance(11 * time.Hour)
	if w := do(e.admin, "GET", "/queue", "", withCookie(c)); w.Code != 200 {
		t.Fatalf("before expiry: %d", w.Code)
	}
	e.clock.Advance(2 * time.Hour)
	if w := do(e.admin, "GET", "/queue", "", withCookie(c)); w.Code != 401 {
		t.Fatalf("after expiry: %d", w.Code)
	}
}

func TestLoginRejectsBadInput(t *testing.T) {
	e := newEnv(t)
	for _, b := range []string{``, `{}`, `{"key":""}`, `{"key":"nope"}`, `not json`, `{"key":5}`} {
		w := do(e.admin, "POST", "/login", b, remote("198.51.100.77:1"))
		if w.Code != 401 && w.Code != 400 {
			t.Errorf("login %q = %d", b, w.Code)
		}
		if len(w.Result().Cookies()) != 0 {
			t.Errorf("cookie issued for %q", b)
		}
		e.clock.Advance(time.Hour)
	}
	if w := do(e.admin, "POST", "/login", `{"key":"`+strings.Repeat("a", 5000)+`"}`); w.Code != 413 {
		t.Errorf("oversized login = %d", w.Code)
	}
}

func TestFailedAuthBackoff(t *testing.T) {
	e := newEnv(t, func(d *Deps) {
		d.Config.AuthFreeAttempts = 2
		d.Config.AuthBackoffBase = time.Second
		d.Config.AuthBackoffMax = 8 * time.Second
	})
	bad := func(ip string) *httptest.ResponseRecorder {
		return do(e.admin, "GET", "/queue", "", hdr("X-Admin-Key", "wrong-key-wrong-key"), remote(ip))
	}
	good := func(ip string) int { return do(e.admin, "GET", "/queue", "", withKey(), remote(ip)).Code }

	const a, b = "198.51.100.1:1", "198.51.100.2:1"
	// two free failures, then lockouts of 1s, 2s, 4s, 8s, 8s
	if bad(a).Code != 401 || bad(a).Code != 401 {
		t.Fatal("free attempts should be plain 401")
	}
	if bad(a).Code != 401 { // third failure triggers the first (1s) lockout
		t.Fatal("third failure")
	}
	w := bad(a)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("expected 429 + Retry-After, got %d", w.Code)
	}
	// While locked even the CORRECT key is refused (no oracle for brute force).
	if c := good(a); c != 429 {
		t.Errorf("correct key during lockout = %d, want 429", c)
	}
	// Another IP is unaffected.
	if c := good(b); c != 200 {
		t.Errorf("other IP = %d", c)
	}
	// Lockout also covers /login.
	if w := do(e.admin, "POST", "/login", `{"key":"`+testKey+`"}`, remote(a)); w.Code != 429 {
		t.Errorf("login during lockout = %d", w.Code)
	}
	// After the delay the next failure doubles the lockout.
	e.clock.Advance(1100 * time.Millisecond)
	if bad(a).Code != 401 {
		t.Fatal("post-lockout failure should be evaluated")
	}
	e.clock.Advance(1500 * time.Millisecond) // < 2s
	if bad(a).Code != 429 {
		t.Error("lockout should have doubled to 2s")
	}
	e.clock.Advance(1 * time.Second)
	if bad(a).Code != 401 { // 3rd lockout = 4s
		t.Fatal("expected evaluation after 2s")
	}
	e.clock.Advance(3 * time.Second)
	if bad(a).Code != 429 {
		t.Error("lockout should be 4s")
	}
	// Cap at AuthBackoffMax.
	for i := 0; i < 6; i++ {
		e.clock.Advance(20 * time.Second)
		bad(a)
	}
	retry := bad(a).Header().Get("Retry-After")
	if retry != "8" {
		t.Errorf("Retry-After = %q, want capped at 8", retry)
	}
	// Success resets the counter.
	e.clock.Advance(20 * time.Second)
	if c := good(a); c != 200 {
		t.Fatalf("good after lockout = %d", c)
	}
	for i := 0; i < 2; i++ {
		if bad(a).Code != 401 {
			t.Fatal("counter not reset by success")
		}
	}
}

func TestAuthFailureMapBounded(t *testing.T) {
	e := newEnv(t)
	a := newAuthenticator(e.deps.Config.withDefaults(), e.clock.Now)
	for i := 0; i < maxFailIPs*2; i++ {
		a.recordFailure(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	if len(a.fails) > maxFailIPs {
		t.Errorf("fail map grew to %d", len(a.fails))
	}
	for i := 0; i < maxSessions*2; i++ {
		a.newSession()
	}
	if len(a.sessions) > maxSessions {
		t.Errorf("sessions grew to %d", len(a.sessions))
	}
}

func TestNoCredentialsDoNotCountAsFailures(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 10; i++ {
		do(e.admin, "GET", "/queue", "")
	}
	if c := do(e.admin, "GET", "/queue", "", withKey()).Code; c != 200 {
		t.Errorf("unauthenticated polling locked the IP out: %d", c)
	}
}

// ---------------------------------------------------- headers / isolation

func TestSecurityHeadersAndStrictCSPEverywhere(t *testing.T) {
	e := newEnv(t)
	check := func(label string, w *httptest.ResponseRecorder) {
		h := w.Header()
		csp := h.Get("Content-Security-Policy")
		if csp == "" {
			t.Errorf("%s: CSP missing", label)
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "http") || strings.Contains(csp, "*") {
			t.Errorf("%s: weak CSP %q", label, csp)
		}
		for _, want := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'self'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP lacks %q", label, want)
			}
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: missing security headers: %v", label, h)
		}
	}
	for _, p := range []string{"/admin", "/assets/app.js", "/health", "/nope", "/queue"} {
		check("admin "+p, do(e.admin, "GET", p, ""))
	}
	check("admin 401", do(e.admin, "GET", "/queue", ""))
	check("public 404", do(e.public, "GET", "/admin", ""))
	check("public contact", do(e.public, "POST", "/contact", `{}`))
	check("public preflight", do(e.public, "OPTIONS", "/contact", ""))
}

func TestAdminHidesFromCloudflareTraffic(t *testing.T) {
	e := newEnv(t)
	for _, h := range []string{"CF-Ray", "Cf-Connecting-Ip"} {
		for _, p := range []string{"/admin", "/health", "/queue", "/metrics", "/login", "/"} {
			// even with perfectly valid credentials
			w := do(e.admin, "GET", p, "", hdr(h, "x"), withKey())
			if w.Code != 404 {
				t.Errorf("%s: %s = %d, want 404", h, p, w.Code)
			}
			if strings.Contains(w.Body.String(), "unauthorized") {
				t.Errorf("%s %s leaked auth state", h, p)
			}
		}
	}
	if w := do(e.admin, "POST", "/login", `{"key":"`+testKey+`"}`, hdr("CF-Ray", "1")); w.Code != 404 || len(w.Result().Cookies()) != 0 {
		t.Errorf("login via tunnel = %d", w.Code)
	}
}

func TestAdminUnknownRoutes404(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/nope", "/docs", "/openapi.json", "/admin/x", "/assets/", "/assets/../go.mod", "//admin", "/./admin", "/assets/missing.js"} {
		if w := do(e.admin, "GET", p, "", withKey()); w.Code != 404 {
			t.Errorf("%s = %d", p, w.Code)
		}
	}
}

func TestAssetContentTypes(t *testing.T) {
	e := newEnv(t)
	for p, want := range map[string]string{
		"/assets/app.js": "text/javascript; charset=utf-8", "/assets/app.css": "text/css; charset=utf-8",
		"/admin": "text/html; charset=utf-8",
	} {
		if got := do(e.admin, "GET", p, "").Header().Get("Content-Type"); got != want {
			t.Errorf("%s content-type %q, want %q", p, got, want)
		}
	}
}

func TestRecoverMiddleware(t *testing.T) {
	e := newEnv(t)
	e.queue.panicOn = true
	w := do(e.admin, "GET", "/queue", "", withKey())
	if w.Code != 500 {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "secret-internal-detail") || strings.Contains(body, "goroutine") || strings.Contains(body, ".go:") {
		t.Errorf("panic details leaked: %s", body)
	}
	e.queue.panicOn = false
	if w := do(e.admin, "GET", "/queue", "", withKey()); w.Code != 200 {
		t.Errorf("handler dead after panic: %d", w.Code)
	}
}

// ------------------------------------------------------------ admin API

func TestHealth(t *testing.T) {
	e := newEnv(t)
	var h map[string]any
	jsonBody(t, do(e.admin, "GET", "/health", ""), &h)
	if h["ok"] != true || h["db"] != "ok" || h["printer"] != "up" || h["printer_mode"] != "network" || h["queue_depth"] != float64(0) {
		t.Errorf("health = %v", h)
	}
	e.store.failPing = true
	jsonBody(t, do(e.admin, "GET", "/health", ""), &h)
	if h["ok"] != false || h["db"] != "failed" {
		t.Errorf("health = %v", h)
	}
}

func TestQueueShape(t *testing.T) {
	e := newEnv(t)
	seed(e, 3)
	var q map[string]int
	jsonBody(t, do(e.admin, "GET", "/queue", "", withKey()), &q)
	want := map[string]int{"pending": 3, "total": 3, "printed_this_hour": 0, "in_flight": 0, "limit_per_hour": 10}
	for k, v := range want {
		if got, ok := q[k]; !ok || got != v {
			t.Errorf("queue[%s] = %d (present=%v), want %d", k, got, ok, v)
		}
	}
	if len(q) != len(want) {
		t.Errorf("queue has extra keys: %v", q)
	}
}

func TestDrainClaimsOldestFirstAndSubmits(t *testing.T) {
	e := newEnv(t)
	seed(e, 3)
	e.store.msgs[1].Claimed = 2 // skipped: must not be drained
	w := do(e.admin, "POST", "/drain", "", withKey())
	var r map[string]int
	jsonBody(t, w, &r)
	if w.Code != 200 || r["queued_to_print"] != 2 {
		t.Fatalf("%d %v", w.Code, r)
	}
	jobs := e.queue.snapshot()
	if len(jobs) != 2 || jobs[0].MsgID != 1 || jobs[1].MsgID != 3 || jobs[0].Kind != domain.JobMessage || jobs[0].Msg.ID != 1 {
		t.Errorf("jobs = %+v", jobs)
	}
	if e.store.msgs[0].Claimed != 1 || e.store.msgs[1].Claimed != 2 {
		t.Error("claim states wrong")
	}
	// nothing left
	jsonBody(t, do(e.admin, "POST", "/drain", "", withKey()), &r)
	if r["queued_to_print"] != 0 {
		t.Errorf("second drain = %v", r)
	}
}

func TestPrintAndSkipSemantics(t *testing.T) {
	e := newEnv(t)
	seed(e, 2)
	w := do(e.admin, "POST", "/print/1", "", withKey())
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"queued":true}` {
		t.Fatalf("print = %d %s", w.Code, w.Body)
	}
	if jobs := e.queue.snapshot(); len(jobs) != 1 || jobs[0].MsgID != 1 {
		t.Errorf("jobs = %+v", jobs)
	}
	// already claimed => 404 with the Python text
	w = do(e.admin, "POST", "/print/1", "", withKey())
	var d map[string]string
	jsonBody(t, w, &d)
	if w.Code != 404 || d["detail"] != "message not found or already printed/claimed" {
		t.Errorf("reprint = %d %v", w.Code, d)
	}
	if w := do(e.admin, "POST", "/print/999", "", withKey()); w.Code != 404 {
		t.Errorf("unknown print = %d", w.Code)
	}
	if w := do(e.admin, "POST", "/print/abc", "", withKey()); w.Code != 422 {
		t.Errorf("non-int id = %d", w.Code)
	}

	w = do(e.admin, "POST", "/skip/2", "", withKey())
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"skipped":true}` {
		t.Errorf("skip = %d %s", w.Code, w.Body)
	}
	jsonBody(t, do(e.admin, "POST", "/skip/2", "", withKey()), &d)
	if d["detail"] != "message not found or already printed/claimed" {
		t.Errorf("reskip = %v", d)
	}
	if w := do(e.admin, "POST", "/skip/1", "", withKey()); w.Code != 404 {
		t.Errorf("skip claimed = %d", w.Code)
	}
}

func TestPrintSubmitFailureReleasesClaim(t *testing.T) {
	e := newEnv(t)
	seed(e, 1)
	e.queue.err = fmt.Errorf("closed")
	if w := do(e.admin, "POST", "/print/1", "", withKey()); w.Code != 503 {
		t.Errorf("status %d", w.Code)
	}
	if e.store.msgs[0].Claimed != 0 {
		t.Error("claim leaked")
	}
}

func TestPrintImage(t *testing.T) {
	e := newEnv(t)
	_, b64 := validImage()
	w := do(e.admin, "POST", "/print/image", `{"image_b64":"`+b64+`"}`, withKey()) // default mode
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"mode":"bitmap","queued":true}` {
		t.Fatalf("bitmap = %d %s", w.Code, w.Body)
	}
	w = do(e.admin, "POST", "/print/image", `{"image_b64":"`+b64+`","mode":"ascii"}`, withKey())
	if w.Code != 200 {
		t.Fatalf("ascii = %d %s", w.Code, w.Body)
	}
	jobs := e.queue.snapshot()
	if len(jobs) != 2 || jobs[0].Kind != domain.JobImage || jobs[0].Raster == nil ||
		jobs[1].Kind != domain.JobASCII || jobs[1].Text != "ascii-2x2-62" {
		t.Errorf("jobs = %+v", jobs)
	}
	for body, want := range map[string]string{
		`{"image_b64":"` + b64 + `","mode":"sepia"}`: "mode must be 'bitmap' or 'ascii'",
		`{"image_b64":"@@@"}`:                        "invalid base64",
		`{"image_b64":"aGVsbG8="}`:                   "unreadable image",
		`{"image_b64":""}`:                           "empty image",
	} {
		w := do(e.admin, "POST", "/print/image", body, withKey())
		var d map[string]string
		jsonBody(t, w, &d)
		if w.Code != 400 || d["detail"] != want {
			t.Errorf("%s => %d %v, want 400 %q", body, w.Code, d, want)
		}
	}
	if len(e.queue.snapshot()) != 2 {
		t.Error("rejected image reached the queue")
	}
}

func TestAdminBodyLimitsEnforced(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Config.MaxImageBytes = 1024; d.Config.JSONBodyLimit = 2048 })
	huge := `{"image_b64":"` + strings.Repeat("A", 2*1024+64*1024+16) + `"}`
	for _, p := range []string{"/print/image", "/preview"} {
		if w := do(e.admin, "POST", p, huge, withKey()); w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s oversized = %d, want 413", p, w.Code)
		}
	}
	big := `{"settings":{"receipt_footer":"` + strings.Repeat("a", 4096) + `"}}`
	if w := do(e.admin, "PATCH", "/settings", big, withKey()); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("settings oversized = %d", w.Code)
	}
	// per-field base64 pre-check (400) when under the body cap but above 2x MaxImageBytes
	mid := `{"image_b64":"` + strings.Repeat("A", 2*1024+8) + `"}`
	if w := do(e.admin, "POST", "/print/image", mid, withKey()); w.Code != 400 {
		t.Errorf("mid-size image = %d, want 400 image too large", w.Code)
	}
}

func TestMessagesList(t *testing.T) {
	e := newEnv(t)
	seed(e, 5)
	e.store.msgs[2].Image = []byte("img")
	type listResp struct {
		Total    int                     `json:"total"`
		Messages []domain.MessageSummary `json:"messages"`
	}
	get := func(q string) listResp {
		var r listResp
		w := do(e.admin, "GET", "/messages"+q, "", withKey())
		if w.Code != 200 {
			t.Fatalf("%s = %d", q, w.Code)
		}
		jsonBody(t, w, &r)
		return r
	}
	r := get("")
	if r.Total != 5 || len(r.Messages) != 5 || r.Messages[0].ID != 5 {
		t.Errorf("default list = %+v", r)
	}
	r = get("?limit=2&offset=1")
	if r.Total != 5 || len(r.Messages) != 2 || r.Messages[0].ID != 4 {
		t.Errorf("paged = %+v", r)
	}
	if r := get("?q=PERSON3"); r.Total != 1 || r.Messages[0].ID != 3 || !r.Messages[0].HasImage {
		t.Errorf("search = %+v", r)
	}
	if r := get("?q=example.com&limit=2&offset=2"); r.Total != 5 || len(r.Messages) != 2 || r.Messages[0].ID != 3 {
		t.Errorf("search paged = %+v", r)
	}
	r = get("?q=zzz-none")
	if r.Total != 0 || r.Messages == nil || len(r.Messages) != 0 {
		t.Errorf("empty search = %+v", r)
	}
	if r := get("?limit=-5&offset=-1"); len(r.Messages) != 5 {
		t.Errorf("bad paging not defaulted: %+v", r)
	}
	body := do(e.admin, "GET", "/messages?q=zzz-none", "", withKey()).Body.String()
	if !strings.Contains(body, `"messages":[]`) {
		t.Errorf("messages must be [] not null: %s", body)
	}
}

func TestMessageImage(t *testing.T) {
	e := newEnv(t)
	seed(e, 2)
	raw, _ := validImage()
	e.store.msgs[0].Image = raw
	w := do(e.admin, "GET", "/messages/1/image", "", withKey())
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Body.String() != string(raw) {
		t.Errorf("image = %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if w := do(e.admin, "GET", "/messages/2/image", "", withKey()); w.Code != 404 {
		t.Errorf("no image = %d", w.Code)
	}
	e.store.msgs[1].Image = []byte("<svg onload=alert(1)>")
	w = do(e.admin, "GET", "/messages/2/image", "", withKey())
	if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "html") || strings.Contains(ct, "svg") {
		t.Errorf("active content type served: %q", ct)
	}
}

func TestSettingsGetPatch(t *testing.T) {
	hub := NewHub()
	e := newEnv(t, func(d *Deps) { d.Hub = hub })
	sub := hub.Subscribe()
	defer sub.Close()

	var all map[string]string
	jsonBody(t, do(e.admin, "GET", "/settings", "", withKey()), &all)
	if all["line_width"] != "32" {
		t.Errorf("settings = %v", all)
	}
	w := do(e.admin, "PATCH", "/settings", `{"settings":{"line_width":"40","prints_per_hour":7}}`, withKey())
	jsonBody(t, w, &all)
	if w.Code != 200 || all["line_width"] != "40" || all["prints_per_hour"] != "7" {
		t.Errorf("patch = %d %v", w.Code, all)
	}
	select {
	case ev := <-sub.C:
		if ev.Type != "settings" {
			t.Errorf("event %q", ev.Type)
		}
	case <-time.After(time.Second):
		t.Error("no settings event")
	}
	w = do(e.admin, "PATCH", "/settings", `{"settings":{"bogus":"1"}}`, withKey())
	var d map[string]string
	jsonBody(t, w, &d)
	if w.Code != 400 || !strings.Contains(d["detail"], "Unknown settings") {
		t.Errorf("unknown key = %d %v", w.Code, d)
	}
	for _, b := range []string{`{}`, `nope`, `{"settings":null}`} {
		if w := do(e.admin, "PATCH", "/settings", b, withKey()); w.Code != 422 {
			t.Errorf("patch %q = %d", b, w.Code)
		}
	}
	if w := do(e.admin, "PATCH", "/settings", `{"settings":{"line_width":[1]}}`, withKey()); w.Code != 400 {
		t.Errorf("non-scalar = %d", w.Code)
	}
}

func TestPreview(t *testing.T) {
	e := newEnv(t)
	_, b64 := validImage()
	for _, body := range []string{
		`{"kind":"message","name":"Ada","email":"a@b.co","message":"hello"}`,
		`{"kind":"image","image_b64":"` + b64 + `","mode":"bitmap"}`,
		`{"kind":"image","image_b64":"` + b64 + `","mode":"ascii"}`,
	} {
		w := do(e.admin, "POST", "/preview", body, withKey())
		if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(w.Body.String(), "\x89PNG") {
			t.Errorf("%s => %d %q", body, w.Code, w.Header().Get("Content-Type"))
		}
	}
	for body, code := range map[string]int{
		`{"kind":"other"}`:                                      400,
		`{"kind":"image","image_b64":"@@"}`:                     400,
		`{"kind":"image","image_b64":"aGk="}`:                   400,
		`{"kind":"image","image_b64":"` + b64 + `","mode":"x"}`: 400,
		`garbage`: 422,
	} {
		if w := do(e.admin, "POST", "/preview", body, withKey()); w.Code != code {
			t.Errorf("%s => %d, want %d", body, w.Code, code)
		}
	}
}

// ---------------------------------------------------------------- metrics

func TestMetricsEndpoint(t *testing.T) {
	m := metrics.New()
	e := newEnv(t, func(d *Deps) { d.Metrics = m; d.Stats = fakeStats{ok: 4, failed: 2} })
	seed(e, 1)
	do(e.admin, "GET", "/queue", "", withKey())
	do(e.admin, "GET", "/messages/1/image", "", withKey()) // 404 route template, not raw path
	do(e.admin, "GET", "/messages/999/image", "", withKey())
	do(e.admin, "GET", "/totally/unknown/"+strings.Repeat("x", 50), "")
	e.queue.jobs = make([]domain.Job, 3)

	w := do(e.admin, "GET", "/metrics", "", hdr("Authorization", "Bearer "+testKey))
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("metrics = %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	for _, want := range []string{
		`http_requests_total{handler="/queue",method="GET",status="2xx"} 1`,
		`http_requests_total{handler="/messages/{id}/image",method="GET",status="4xx"} 2`,
		`http_requests_total{handler="unmatched",method="GET",status="4xx"} 1`,
		`printer_prints_total{result="success"} 4`,
		`printer_prints_total{result="failed"} 2`,
		`printer_queue_depth 3`,
		`printer_connected 1`,
		`printer_messages_total{result="accepted"} 0`,
		"go_goroutines ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	if strings.Contains(body, "999") || strings.Contains(body, "xxxxx") {
		t.Error("raw paths leaked into labels (cardinality!)")
	}
}

func TestPrintCountersFromHubEvents(t *testing.T) {
	m := metrics.New()
	hub := NewHub()
	newEnv(t, func(d *Deps) { d.Metrics = m; d.Hub = hub })
	hub.Publish(domain.Event{Type: "print", Data: PrintEvent{ID: 1, OK: true}})
	hub.Publish(domain.Event{Type: "print", Data: map[string]any{"id": 2, "ok": false, "error": "x"}})
	hub.Publish(domain.Event{Type: "print", Data: &PrintEvent{ID: 3, OK: true}})
	hub.Publish(domain.Event{Type: "status", Data: 1})
	if m.Prints.Value("success") != 2 || m.Prints.Value("failed") != 1 {
		t.Errorf("success=%v failed=%v", m.Prints.Value("success"), m.Prints.Value("failed"))
	}
}

// -------------------------------------------------------------------- SSE

func TestSSEReceivesEvents(t *testing.T) {
	hub := NewHub()
	e := newEnv(t, func(d *Deps) { d.Hub = hub; d.Config.StatusInterval = 50 * time.Millisecond })
	seed(e, 1)
	srv := httptest.NewServer(e.admin)
	defer srv.Close()

	// unauthenticated => 401, no stream
	resp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated SSE = %d", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/events", nil)
	req.Header.Set("X-Admin-Key", testKey)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("SSE = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Error("CSP missing on SSE")
	}

	type frame struct{ event, data string }
	frames := make(chan frame, 32)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		var f frame
		for sc.Scan() {
			switch line := sc.Text(); {
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			case line == "" && f.event != "":
				frames <- f
				f = frame{}
			}
		}
		close(frames)
	}()
	waitFor := func(event string) frame {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case f, ok := <-frames:
				if !ok {
					t.Fatalf("stream closed waiting for %q", event)
				}
				if f.event == event {
					return f
				}
			case <-deadline:
				t.Fatalf("timeout waiting for %q", event)
			}
		}
	}

	st := waitFor("status") // initial status
	var sd map[string]any
	if err := json.Unmarshal([]byte(st.data), &sd); err != nil || sd["pending"] != float64(1) || sd["printer"] == nil {
		t.Errorf("status data = %s (%v)", st.data, err)
	}
	// Wait until the subscription exists, then publish.
	for i := 0; i < 100 && hub.Len() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	hub.Publish(domain.Event{Type: "print", Data: PrintEvent{ID: 7, OK: false, Error: "paper out"}})
	f := waitFor("print")
	if f.data != `{"id":7,"ok":false,"error":"paper out"}` {
		t.Errorf("print data = %s", f.data)
	}
	// A contact submission surfaces as a "message" event on the admin stream.
	do(e.public, "POST", "/contact", `{"name":"Ada","message":"live!"}`)
	var sum domain.MessageSummary
	if err := json.Unmarshal([]byte(waitFor("message").data), &sum); err != nil || sum.Message != "live!" {
		t.Errorf("message event = %+v (%v)", sum, err)
	}
	waitFor("status") // periodic keep-alive status
}

func TestSSEClientDisconnectUnsubscribes(t *testing.T) {
	hub := NewHub()
	e := newEnv(t, func(d *Deps) { d.Hub = hub })
	srv := httptest.NewServer(e.admin)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/events", nil)
	req.Header.Set("X-Admin-Key", testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100 && hub.Len() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if hub.Len() != 1 {
		t.Fatalf("subscribers = %d", hub.Len())
	}
	resp.Body.Close()
	for i := 0; i < 200 && hub.Len() != 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if hub.Len() != 0 {
		t.Error("subscription leaked after disconnect")
	}
}

func TestSSECapReturns503(t *testing.T) {
	hub := NewHubSize(4, 1)
	e := newEnv(t, func(d *Deps) { d.Hub = hub })
	hold := hub.Subscribe()
	defer hold.Close()
	if w := do(e.admin, "GET", "/events", "", withKey()); w.Code != 503 {
		t.Errorf("status %d", w.Code)
	}
}
