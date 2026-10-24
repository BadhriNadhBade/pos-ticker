package httpapi

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cookieName   = "pt_session"
	csrfHeader   = "X-Requested-With"
	csrfValue    = "pos-ticker"
	maxSessions  = 256
	maxFailIPs   = 4096
	failIdleTime = time.Hour
)

type failState struct {
	fails    int
	until    time.Time // blocked until
	lastSeen time.Time
}

// authenticator implements admin-key checking, per-IP failed-auth backoff and
// HMAC-signed server-side sessions.
type authenticator struct {
	keyHash [32]byte
	secret  []byte
	ttl     time.Duration
	now     func() time.Time
	free    int
	base    time.Duration
	max     time.Duration

	mu       sync.Mutex
	sessions map[string]*session // token (b64) -> session
	fails    map[string]*failState
}

// session is one logged-in browser. done is closed when the session is
// revoked, evicted or found expired, which ends that session's SSE streams.
type session struct {
	exp  time.Time
	done chan struct{}
}

func newAuthenticator(cfg Config, now func() time.Time) *authenticator {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		panic("httpapi: crypto/rand failed: " + err.Error())
	}
	return &authenticator{
		keyHash:  sha256.Sum256([]byte(cfg.AdminKey)),
		secret:   secret,
		ttl:      cfg.SessionTTL,
		now:      now,
		free:     cfg.AuthFreeAttempts,
		base:     cfg.AuthBackoffBase,
		max:      cfg.AuthBackoffMax,
		sessions: map[string]*session{},
		fails:    map[string]*failState{},
	}
}

// keyMatches compares in constant time. Both sides are hashed first so the
// comparison length never depends on the supplied value.
func (a *authenticator) keyMatches(got string) bool {
	h := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(h[:], a.keyHash[:]) == 1
}

// retryAfter returns how long ip must wait before another attempt is even
// evaluated (0 = not blocked).
func (a *authenticator) retryAfter(ip string) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	if f, ok := a.fails[ip]; ok {
		if d := f.until.Sub(a.now()); d > 0 {
			return d
		}
	}
	return 0
}

// tryKey checks the backoff, compares the key and records the outcome under a
// single lock, so concurrent guesses cannot all slip through the same
// "not blocked yet" window. wait > 0 means the caller is locked out (the key
// was NOT evaluated); otherwise ok says whether the key matched.
func (a *authenticator) tryKey(ip, got string) (ok bool, wait time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if f, found := a.fails[ip]; found {
		if d := f.until.Sub(now); d > 0 {
			return false, d
		}
	}
	if a.keyMatches(got) {
		delete(a.fails, ip)
		return true, 0
	}
	a.recordFailureLocked(ip, now)
	return false, 0
}

// recordFailure registers a failed attempt and returns the new lockout.
func (a *authenticator) recordFailure(ip string) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.recordFailureLocked(ip, a.now())
}

func (a *authenticator) recordFailureLocked(ip string, now time.Time) time.Duration {
	f, ok := a.fails[ip]
	if !ok {
		if len(a.fails) >= maxFailIPs {
			a.evictFailsLocked(now)
		}
		f = &failState{}
		a.fails[ip] = f
	}
	f.fails++
	f.lastSeen = now
	var d time.Duration
	if n := f.fails - a.free; n > 0 {
		if n > 30 {
			n = 30
		}
		d = a.base << (n - 1)
		if d <= 0 || d > a.max {
			d = a.max
		}
	}
	f.until = now.Add(d)
	return d
}

// evictFailsLocked keeps the failure map bounded: drop idle entries, and if
// that is not enough drop the least recently seen one.
func (a *authenticator) evictFailsLocked(now time.Time) {
	var oldestIP string
	var oldest time.Time
	for ip, f := range a.fails {
		if now.Sub(f.lastSeen) > failIdleTime && now.After(f.until) {
			delete(a.fails, ip)
			continue
		}
		if oldestIP == "" || f.lastSeen.Before(oldest) {
			oldestIP, oldest = ip, f.lastSeen
		}
	}
	if len(a.fails) >= maxFailIPs && oldestIP != "" {
		delete(a.fails, oldestIP)
	}
}

func (a *authenticator) sign(token string) string {
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte(token))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// newSession creates a session and returns the cookie value and expiry.
func (a *authenticator) newSession() (value string, exp time.Time) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic("httpapi: crypto/rand failed: " + err.Error())
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := a.now()
	exp = now.Add(a.ttl)

	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.sessions) >= maxSessions {
		var oldestTok string
		var oldestExp time.Time
		for t, s := range a.sessions {
			if !s.exp.After(now) {
				a.dropLocked(t)
				continue
			}
			if oldestTok == "" || s.exp.Before(oldestExp) {
				oldestTok, oldestExp = t, s.exp
			}
		}
		if len(a.sessions) >= maxSessions && oldestTok != "" {
			a.dropLocked(oldestTok)
		}
	}
	a.sessions[token] = &session{exp: exp, done: make(chan struct{})}
	return token + "." + a.sign(token), exp
}

// dropLocked removes a session and wakes everything streaming under it.
func (a *authenticator) dropLocked(token string) {
	if s, ok := a.sessions[token]; ok {
		delete(a.sessions, token)
		close(s.done)
	}
}

func (a *authenticator) parseCookie(value string) (token string, ok bool) {
	token, sig, found := strings.Cut(value, ".")
	if !found || token == "" {
		return "", false
	}
	if !hmac.Equal([]byte(sig), []byte(a.sign(token))) {
		return "", false
	}
	return token, true
}

func (a *authenticator) validSession(value string) bool {
	_, ok := a.sessionDone(value)
	return ok
}

// sessionDone validates the cookie value and returns the channel that is
// closed when that session ends.
func (a *authenticator) sessionDone(value string) (<-chan struct{}, bool) {
	token, ok := a.parseCookie(value)
	if !ok {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[token]
	if !ok {
		return nil, false
	}
	if !a.now().Before(s.exp) {
		a.dropLocked(token)
		return nil, false
	}
	return s.done, true
}

func (a *authenticator) revoke(value string) {
	if token, ok := a.parseCookie(value); ok {
		a.mu.Lock()
		a.dropLocked(token)
		a.mu.Unlock()
	}
}

// ------------------------------------------------------------ HTTP plumbing

func (s *server) setSessionCookie(w http.ResponseWriter, r *http.Request, value string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		Expires:  exp,
		MaxAge:   int(s.cfg.SessionTTL / time.Second),
		HttpOnly: true,
		Secure:   isHTTPS(r, s.cfg.TrustProxy),
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isHTTPS(r, s.cfg.TrustProxy),
		SameSite: http.SameSiteStrictMode,
	})
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func tooManyAttempts(w http.ResponseWriter, wait time.Duration) {
	secs := int((wait + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeDetail(w, http.StatusTooManyRequests, "too many failed attempts")
}

// authorize enforces authentication for an admin route. It writes the error
// response itself and returns false when the request must not proceed.
//
//   - X-Admin-Key header (and "Authorization: Bearer" when allowBearer):
//     constant-time compare, per-IP backoff on failure, exempt from CSRF.
//   - pt_session cookie: HMAC-verified, and for mutating methods the
//     X-Requested-With: pos-ticker header is mandatory (CSRF defence).
func (s *server) authorize(w http.ResponseWriter, r *http.Request, allowBearer bool) bool {
	ip := s.clientIP(r)

	key := r.Header.Get("X-Admin-Key")
	if key == "" && allowBearer {
		key = bearer(r)
	}
	if key != "" {
		ok, wait := s.auth.tryKey(ip, key)
		if wait > 0 {
			tooManyAttempts(w, wait)
			return false
		}
		if ok {
			return true
		}
		s.log.Warn("failed admin auth", "ip", ip, "path", r.URL.Path)
		writeDetail(w, http.StatusUnauthorized, "unauthorized")
		return false
	}

	if c, err := r.Cookie(cookieName); err == nil && s.auth.validSession(c.Value) {
		if !isSafeMethod(r.Method) && r.Header.Get(csrfHeader) != csrfValue {
			writeDetail(w, http.StatusForbidden, "missing CSRF header")
			return false
		}
		return true
	}
	writeDetail(w, http.StatusUnauthorized, "unauthorized")
	return false
}

func (s *server) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.authorize(w, r, false) {
			h(w, r)
		}
	}
}

// handleLogin exchanges the admin key for a session cookie.
func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	// Cheap early refusal (before reading the body); tryKey below re-checks
	// the lockout atomically with the compare-and-record.
	if wait := s.auth.retryAfter(ip); wait > 0 {
		tooManyAttempts(w, wait)
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(w, r, 4096, &body); err != nil {
		if isTooLarge(err) {
			writeDetail(w, http.StatusRequestEntityTooLarge, "request too large")
			return
		}
		writeDetail(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	ok, wait := s.auth.tryKey(ip, body.Key)
	if wait > 0 {
		tooManyAttempts(w, wait)
		return
	}
	if !ok { // an empty key never matches (AdminKey is >= 16 chars)
		s.log.Warn("failed admin login", "ip", ip)
		writeDetail(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	value, exp := s.auth.newSession()
	s.setSessionCookie(w, r, value, exp)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleLogout revokes the session. Under cookie auth it needs the CSRF header
// like every other mutating request.
func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		if r.Header.Get("X-Admin-Key") == "" && r.Header.Get(csrfHeader) != csrfValue {
			writeDetail(w, http.StatusForbidden, "missing CSRF header")
			return
		}
		s.auth.revoke(c.Value)
	}
	s.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
