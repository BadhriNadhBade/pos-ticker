package httpapi

import (
	"encoding/json"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
	"github.com/badhrinadhbade/pos-ticker/internal/metrics"
)

type server struct {
	d    Deps
	cfg  Config
	log  *slog.Logger
	hub  *Hub
	m    *metrics.Metrics
	auth *authenticator // admin only
	now  func() time.Time

	// trustProxy: honour forwarded headers for the client IP. Only ever true
	// on the PUBLIC handler (and then only for loopback/private peers).
	trustProxy bool

	// public-path abuse controls
	ipHits      *slidingCounter // accepted messages per IP key, 1h (mirrors max_per_ip_hour)
	imgTries    *slidingCounter // image-bearing attempts per IP key, 1m
	imageSem    chan struct{}   // bounds concurrent PrepareImage on the public path
	countCached countCache
}

func newServer(d Deps, public bool) *server {
	return &server{
		d: d, cfg: d.Config, log: d.Logger, hub: d.Hub, m: d.Metrics, now: d.Now,
		trustProxy: public && d.Config.TrustProxy,
		ipHits:     newSlidingCounter(time.Hour),
		imgTries:   newSlidingCounter(time.Minute),
		imageSem:   make(chan struct{}, imageConcurrency),
	}
}

// NewPublicHandler builds the handler for the PUBLIC listener (PUBLIC_ADDR):
// POST /contact, GET /contact/count and CORS preflight for those two paths.
// Every other path/method is a 404.
func NewPublicHandler(d Deps) (http.Handler, error) {
	d, err := d.prepare(false)
	if err != nil {
		return nil, err
	}
	s := newServer(d, true)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /contact", s.handleContact)
	mux.HandleFunc("GET /contact/count", s.handleContactCount)
	mux.HandleFunc("OPTIONS /contact", s.handlePreflight)
	mux.HandleFunc("OPTIONS /contact/count", s.handlePreflight)
	mux.HandleFunc("/", notFound) // also swallows wrong methods => 404, not 405

	var h http.Handler = s.cors(cleanPaths(mux))
	h = recoverer(s.log, h)
	h = instrument(s.m, h)
	return securityHeaders(h), nil
}

// NewAdminHandler builds the handler for the ADMIN listener (ADMIN_ADDR). It
// fails if Config.AdminKey is shorter than 16 characters. Requests that carry
// CF-Ray or Cf-Connecting-Ip (i.e. came through the Cloudflare tunnel) get a
// 404 before anything else happens.
func NewAdminHandler(d Deps) (http.Handler, error) {
	d, err := d.prepare(true)
	if err != nil {
		return nil, err
	}
	s := newServer(d, false) // admin: socket address only, never forwarded headers
	s.auth = newAuthenticator(s.cfg, s.now)

	if d.Stats == nil {
		s.hub.Observe(s.countPrintEvents)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin", http.StatusFound)
	})
	mux.HandleFunc("GET /admin", s.handleUI)
	mux.HandleFunc("GET /assets/", s.handleAsset)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)

	mux.HandleFunc("GET /events", s.authed(s.handleEvents))
	mux.HandleFunc("GET /queue", s.authed(s.handleQueue))
	mux.HandleFunc("POST /drain", s.authed(s.handleDrain))
	mux.HandleFunc("POST /print/image", s.authed(s.handlePrintImage))
	mux.HandleFunc("POST /print/{id}", s.authed(s.handlePrintMessage))
	mux.HandleFunc("POST /skip/{id}", s.authed(s.handleSkip))
	mux.HandleFunc("GET /messages", s.authed(s.handleMessages))
	mux.HandleFunc("GET /messages/{id}/image", s.authed(s.handleMessageImage))
	mux.HandleFunc("GET /settings", s.authed(s.handleGetSettings))
	mux.HandleFunc("PATCH /settings", s.authed(s.handlePatchSettings))
	mux.HandleFunc("POST /preview", s.authed(s.handlePreview))
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		if s.authorize(w, r, true) {
			s.handleMetrics(w, r)
		}
	})
	mux.HandleFunc("/", notFound)

	var h http.Handler = s.hostGuard(s.cfGuard(cleanPaths(mux)))
	h = recoverer(s.log, h)
	h = instrument(s.m, h)
	return securityHeaders(h), nil
}

// hostGuard enforces Config.AllowedHosts (DNS-rebinding defence). With an
// empty list every Host is accepted. Otherwise the Host (port stripped) must
// be an IP literal, "localhost", or a listed name.
func (s *server) hostGuard(next http.Handler) http.Handler {
	if len(s.cfg.AllowedHosts) == 0 {
		return next
	}
	allowed := make(map[string]bool, len(s.cfg.AllowedHosts))
	for _, h := range s.cfg.AllowedHosts {
		if h = hostOnly(strings.TrimSpace(h)); h != "" {
			allowed[strings.ToLower(h)] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := strings.ToLower(hostOnly(r.Host))
		if h == "" || !(h == "localhost" || net.ParseIP(h) != nil || allowed[h]) {
			writeDetail(w, http.StatusForbidden, "invalid host")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostOnly strips an optional port and IPv6 brackets from a Host value.
func hostOnly(hp string) string {
	if h, _, err := net.SplitHostPort(hp); err == nil {
		return h
	}
	return strings.Trim(hp, "[]")
}

// cfGuard hides the admin surface from tunnelled traffic (defence in depth).
func (s *server) cfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("CF-Ray") != "" || r.Header.Get("Cf-Connecting-Ip") != "" {
			notFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// countPrintEvents derives printer_prints_total from "print" hub events when
// no Stats provider is wired.
func (s *server) countPrintEvents(ev domain.Event) {
	if ev.Type != "print" {
		return
	}
	var ok bool
	switch v := ev.Data.(type) {
	case PrintEvent:
		ok = v.OK
	case *PrintEvent:
		ok = v != nil && v.OK
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		var p struct {
			OK bool `json:"ok"`
		}
		if json.Unmarshal(b, &p) != nil {
			return
		}
		ok = p.OK
	}
	if ok {
		s.m.Print(metrics.PrintSuccess)
	} else {
		s.m.Print(metrics.PrintFailed)
	}
}

// ---------------------------------------------------------------- static UI

func (s *server) handleUI(w http.ResponseWriter, r *http.Request) {
	if s.d.UI == nil {
		writeDetail(w, http.StatusNotFound, "UI not available")
		return
	}
	b, err := fs.ReadFile(s.d.UI, "index.html")
	if err != nil {
		writeDetail(w, http.StatusNotFound, "UI not available")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".txt":   "text/plain; charset=utf-8",
	".map":   "application/json",
}

// handleAsset serves files from the embedded UI FS under /assets/. Directory
// listings are never produced and paths must be valid fs paths (no "..").
func (s *server) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	if s.d.UI == nil || name == "" || !fs.ValidPath(name) {
		notFound(w, r)
		return
	}
	b, err := fs.ReadFile(s.d.UI, name)
	if err != nil {
		notFound(w, r)
		return
	}
	ct, ok := contentTypes[strings.ToLower(path.Ext(name))]
	if !ok {
		ct = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}
