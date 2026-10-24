// Package httpapi implements the two HTTP surfaces of pos-ticker:
//
//   - the PUBLIC handler (NewPublicHandler): POST /contact, GET /contact/count,
//     CORS preflight and nothing else (this is what the Cloudflare tunnel
//     points at), and
//   - the ADMIN handler (NewAdminHandler): UI, authenticated JSON API, SSE
//     and /metrics (never exposed through the tunnel).
//
// It depends only on the interfaces in internal/domain plus the small seams
// declared here (Mailer, Stats) and the Hub it implements itself.
package httpapi

import (
	"errors"
	"io/fs"
	"log/slog"
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
	"github.com/badhrinadhbade/pos-ticker/internal/metrics"
)

// Mailer sends the optional "new message" e-mail notification.
// It is always invoked from its own goroutine, never on the request path.
type Mailer interface {
	Notify(name, email, message, ip, timestamp string)
}

// Stats exposes printer counters owned by the printer worker. If nil, print
// success/failure counters are derived from "print" events published on the Hub.
type Stats interface {
	PrintTotals() (success, failed uint64)
}

// Config holds the static (env-derived) settings of the HTTP layer. Zero
// values get sensible defaults, except AdminKey which is mandatory for the
// admin handler (>= MinAdminKeyLen chars).
type Config struct {
	AdminKey string
	// TrustProxy makes the PUBLIC handler honour Cf-Connecting-Ip / the
	// rightmost X-Forwarded-For hop, but only when the socket peer is
	// loopback/private. The ADMIN handler never trusts forwarded headers.
	TrustProxy bool
	// AllowedHosts, when non-empty, restricts the ADMIN handler to requests
	// whose Host header is an IP literal, "localhost" or one of these names
	// (case-insensitive, port ignored). Empty = accept any Host. Defends the
	// admin UI against DNS rebinding. main wires ADMIN_ALLOWED_HOSTS to it.
	AllowedHosts    []string
	MaxImageBytes   int            // default 10 MiB
	MaxMessageChars int            // default 500 (runes)
	ASCIIColumns    int            // default 62
	DisplayTZ       *time.Location // default America/Los_Angeles (UTC if tzdata is unavailable)

	SessionTTL       time.Duration // default 12h
	StatusInterval   time.Duration // SSE periodic status event, default 10s
	AuthFreeAttempts int           // failed attempts per IP before backoff starts, default 3
	AuthBackoffBase  time.Duration // first backoff step, doubled per failure, default 1s
	AuthBackoffMax   time.Duration // default 15m
	JSONBodyLimit    int64         // small admin JSON bodies, default 1 MiB
	SSEMaxLifetime   time.Duration // max age of one /events stream, default 1h (client reconnects)
}

// MinAdminKeyLen is the minimum accepted ADMIN_KEY length.
const MinAdminKeyLen = 16

func (c Config) withDefaults() Config {
	if c.MaxImageBytes <= 0 {
		c.MaxImageBytes = 10 * 1024 * 1024
	}
	if c.MaxMessageChars <= 0 {
		c.MaxMessageChars = 500
	}
	if c.ASCIIColumns <= 0 {
		c.ASCIIColumns = 62
	}
	if c.DisplayTZ == nil {
		loc, err := time.LoadLocation("America/Los_Angeles")
		if err != nil {
			loc = time.UTC
		}
		c.DisplayTZ = loc
	}
	if c.SessionTTL <= 0 {
		c.SessionTTL = 12 * time.Hour
	}
	if c.StatusInterval <= 0 {
		c.StatusInterval = 10 * time.Second
	}
	if c.AuthFreeAttempts <= 0 {
		c.AuthFreeAttempts = 3
	}
	if c.AuthBackoffBase <= 0 {
		c.AuthBackoffBase = time.Second
	}
	if c.AuthBackoffMax <= 0 {
		c.AuthBackoffMax = 15 * time.Minute
	}
	if c.JSONBodyLimit <= 0 {
		c.JSONBodyLimit = 1 << 20
	}
	if c.SSEMaxLifetime <= 0 {
		c.SSEMaxLifetime = time.Hour
	}
	return c
}

// bodyLimit is the request body cap for endpoints that carry a base64 image:
// MaxImageBytes*4/3 + 64 KiB (base64 expansion plus JSON envelope).
func (c Config) bodyLimit() int64 { return int64(c.MaxImageBytes)*4/3 + 64*1024 }

// maxBase64Len is the longest base64 text that can encode MaxImageBytes.
func (c Config) maxBase64Len() int { return (c.MaxImageBytes + 2) / 3 * 4 }

// Deps are the collaborators of both handlers. Store, Settings, Queue and
// Renderer are required. Share one Hub and one Metrics between the public and
// admin handlers (and with the printer worker's notify callback).
type Deps struct {
	Store    domain.Store
	Settings domain.Settings
	Queue    domain.Queue
	Renderer domain.Renderer

	Mailer  Mailer           // optional
	Stats   Stats            // optional
	Hub     *Hub             // optional; a private one is created if nil
	Metrics *metrics.Metrics // optional; a private one is created if nil
	UI      fs.FS            // web.FS (index.html, app.js, app.css at root); admin only
	Logger  *slog.Logger     // optional
	Config  Config
	Now     func() time.Time // optional clock, for tests
}

func (d Deps) prepare(needAdmin bool) (Deps, error) {
	switch {
	case d.Store == nil:
		return d, errors.New("httpapi: Deps.Store is required")
	case d.Settings == nil:
		return d, errors.New("httpapi: Deps.Settings is required")
	case d.Queue == nil:
		return d, errors.New("httpapi: Deps.Queue is required")
	case d.Renderer == nil:
		return d, errors.New("httpapi: Deps.Renderer is required")
	}
	if needAdmin && len(d.Config.AdminKey) < MinAdminKeyLen {
		return d, errors.New("httpapi: ADMIN_KEY must be at least 16 characters")
	}
	d.Config = d.Config.withDefaults()
	if d.Hub == nil {
		d.Hub = NewHub()
	}
	if d.Metrics == nil {
		d.Metrics = metrics.New()
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return d, nil
}
