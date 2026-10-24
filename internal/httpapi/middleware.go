package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/metrics"
)

// CSP is sent on every response: no inline script/style, no third parties.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' blob: data:; " +
	"connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// NewServer returns an http.Server with the timeouts required by the contract.
// SSE handlers clear their own write deadline via http.ResponseController.
func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}

// statusRecorder captures the status code. It forwards Flush and exposes
// Unwrap so http.ResponseController (SSE flushing, deadlines) keeps working.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if !s.wrote {
		s.status = http.StatusOK
		s.wrote = true
	}
	return s.ResponseWriter.Write(p)
}

func (s *statusRecorder) Flush() {
	if !s.wrote {
		s.status = http.StatusOK
		s.wrote = true
	}
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// securityHeaders sets the headers required on every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", CSP)
		next.ServeHTTP(w, r)
	})
}

// recoverer converts panics into a generic 500 without leaking details.
func recoverer(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			log.Error("panic in handler", "method", r.Method, "path", r.URL.Path,
				"panic", rec, "stack", string(debug.Stack()))
			if sr, ok := w.(*statusRecorder); !ok || !sr.wrote {
				writeDetail(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// instrument records http_requests_total by route TEMPLATE (the mux pattern),
// never by raw path, so label cardinality is bounded by the route table.
func instrument(m *metrics.Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			// r.Pattern is filled in by the mux on this same *Request.
			m.ObserveHTTP(r.Method, routeLabel(r.Pattern), rec.status)
		}()
		next.ServeHTTP(rec, r)
	})
}

func routeLabel(pattern string) string {
	if pattern == "" || pattern == "/" {
		return "unmatched"
	}
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		pattern = pattern[i+1:]
	}
	return pattern
}

// ------------------------------------------------------------------ helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status, b = http.StatusInternalServerError, []byte(`{"detail":"internal error"}`)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

// writeDetail writes a FastAPI-style {"detail": msg} error (admin API).
func writeDetail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"detail": msg})
}

// writeErr writes the {"error": msg} shape used by /contact.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	writeDetail(w, http.StatusNotFound, "Not Found")
}

// decodeJSON reads a size-limited JSON body into v. Callers must pass a typed
// struct (never map[string]any) so unknown keys are skipped, not materialised.
func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	return json.NewDecoder(r.Body).Decode(v)
}

func isTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// clientIP selects the client address used for rate limiting and stored with
// a message. The socket peer is the default. Only when s.trustProxy is set
// (PUBLIC handler with Config.TrustProxy; never the admin handler) AND the
// socket peer is loopback/private (i.e. the local tunnel/proxy) are forwarded
// headers consulted: Cf-Connecting-Ip first, else the RIGHTMOST
// X-Forwarded-For hop (the one our own proxy appended; earlier hops are
// client-controlled). Each must parse as an IP, otherwise it is ignored.
// IPv6 addresses are collapsed to their /64 (see ipKey).
func (s *server) clientIP(r *http.Request) string {
	peer := peerIP(r)
	if s.trustProxy && peer != nil && isLocalPeer(peer) {
		if v := strings.TrimSpace(r.Header.Get("Cf-Connecting-Ip")); v != "" {
			if ip := net.ParseIP(v); ip != nil {
				return ipKey(ip)
			}
		}
		if v := strings.Join(r.Header.Values("X-Forwarded-For"), ","); v != "" {
			last := v
			if i := strings.LastIndexByte(v, ','); i >= 0 {
				last = v[i+1:]
			}
			if ip := net.ParseIP(strings.TrimSpace(last)); ip != nil {
				return ipKey(ip)
			}
		}
	}
	if peer != nil {
		return ipKey(peer)
	}
	if r.RemoteAddr == "" {
		return "unknown"
	}
	return r.RemoteAddr
}

// peerIP parses the socket address of the request (nil if it is not an IP).
func peerIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if i := strings.IndexByte(host, '%'); i >= 0 { // zone
		host = host[:i]
	}
	return net.ParseIP(host)
}

// isLocalPeer reports whether ip may legitimately be our own reverse proxy /
// tunnel: loopback, private (RFC 1918 / ULA) or link-local.
func isLocalPeer(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// ipKey is the canonical string for an address: IPv4 as-is, IPv6 reduced to
// its /64 network ("2001:db8:1:2::/64") since one subscriber typically owns a
// whole /64 and could otherwise rotate through 2^64 addresses.
func ipKey(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// uncleanPath reports paths with empty or dot segments ("//", "/./", "/../").
// net/http's mux would answer those with a redirect to the cleaned path; both
// listeners answer 404 instead so nothing is revealed or normalised.
func uncleanPath(p string) bool {
	p = strings.TrimSuffix(p, "/")
	for _, seg := range strings.Split(p, "/")[1:] {
		if seg == "" || seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// cleanPaths rejects unclean paths with a 404 before routing.
func cleanPaths(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if uncleanPath(r.URL.Path) {
			notFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isHTTPS reports whether the request reached us over TLS, directly or
// via a trusted proxy that says so.
func isHTTPS(r *http.Request, trustProxy bool) bool {
	if r.TLS != nil {
		return true
	}
	return trustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
