// Package domain holds the shared types and seams between packages.
// It imports only the standard library. Changing anything here requires
// updating every package that implements or consumes it.
package domain

import (
	"context"
	"errors"
	"image"
	"time"
)

var (
	ErrNotFound  = errors.New("not found")      // message missing or not in the required state
	ErrRateLimit = errors.New("rate limited")
)

// TimeLayout is the SQLite/UTC timestamp format shared with the legacy Python DB.
const TimeLayout = "2006-01-02 15:04:05"

// Message mirrors a row of the legacy `messages` table (schema must stay compatible).
// Claimed: 0 = pending, 1 = queued/in-flight (or printed when PrintedAt != nil), 2 = skipped.
type Message struct {
	ID          int64
	Name        string
	Email       string
	Body        string // column `message`
	IP          string
	UserAgent   string
	ReceivedAt  string  // UTC, TimeLayout
	BrowserTime *string // client-supplied display time, sanitized
	PrintedAt   *string // UTC, TimeLayout
	Claimed     int
	HeadersJSON string
	Image       []byte // ORIGINAL upload bytes (may be nil); raster is regenerated at print time
}

// NewMessage is the validated input of POST /contact.
type NewMessage struct {
	Name, Email, Body, IP, UserAgent, HeadersJSON string
	BrowserTime                                   string
	Image                                         []byte
}

// InsertResult is the outcome of the atomic "rate-check + insert + claim" step.
type InsertResult struct {
	ID            int64
	RateLimitedIP bool     // per-IP cap hit: nothing inserted, caller returns 429
	Claimed       *Message // non-nil => a print slot was available and the row is claimed (queue it)
	// Claimed == nil && !RateLimitedIP => stored but left pending (global hourly cap)
}

type MessageSummary struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Email       string  `json:"email"`
	Message     string  `json:"message"`
	IP          string  `json:"ip"`
	ReceivedAt  string  `json:"received_at"`
	BrowserTime *string `json:"browser_time"`
	PrintedAt   *string `json:"printed_at"`
	Headers     *string `json:"headers"`
	Claimed     int     `json:"claimed"`
	HasImage    bool    `json:"has_image"`
}

type QueueCounts struct {
	Pending, Total, PrintedThisHour int
}

// Store is implemented by internal/store (pure-Go SQLite).
type Store interface {
	Ping(ctx context.Context) error
	// InsertMessage atomically: count IP submissions in last hour (>= maxPerIP => RateLimitedIP),
	// insert, and claim the row if print slots used this hour (printed in last hour OR claimed&unprinted) < perHour.
	InsertMessage(ctx context.Context, m NewMessage, maxPerIP, perHour int) (InsertResult, error)
	ClaimPending(ctx context.Context, id int64) (*Message, error)   // ErrNotFound if not pending
	ClaimAllPending(ctx context.Context) ([]Message, error)         // oldest first
	Skip(ctx context.Context, id int64) error                       // claimed=2; ErrNotFound if not pending
	MarkPrinted(ctx context.Context, id int64) error                // printed_at=now, claimed=0
	ReleaseClaim(ctx context.Context, id int64) error               // claimed=0
	ReleaseStaleClaims(ctx context.Context) (int, error)            // startup recovery
	List(ctx context.Context, limit, offset int) ([]MessageSummary, int, error) // newest first, total
	Image(ctx context.Context, id int64) ([]byte, error)            // ErrNotFound if none
	Counts(ctx context.Context) (QueueCounts, error)
	PrintedTotal(ctx context.Context) (int, error)
	LoadSettings(ctx context.Context) (map[string]string, error)
	SaveSettings(ctx context.Context, kv map[string]string) error
	Close() error
}

// Settings is the runtime-editable, SQLite-persisted config (implemented by internal/config).
type Settings interface {
	Get(key string) string
	Int(key string, def int) int
	All() map[string]string
	Patch(ctx context.Context, kv map[string]string) (map[string]string, error) // validates keys + ints
}

// JobKind selects what a print job renders.
type JobKind string

const (
	JobMessage JobKind = "message"
	JobImage   JobKind = "image"
	JobASCII   JobKind = "ascii"
)

// Bitmap is a 1-bit raster already sized to the printer width (true = black).
type Bitmap struct {
	W, H int
	Pix  []bool // len W*H, row-major
}

type Job struct {
	Kind   JobKind
	MsgID  int64
	Msg    *Message
	Raster *Bitmap
	Text   string
}

// Event is pushed to the admin UI over SSE.
type Event struct {
	Type string `json:"type"` // "status" | "message" | "print" | "settings"
	Data any    `json:"data"`
}

// Queue is implemented by internal/printer.
type Queue interface {
	Submit(j Job) error
	Depth() int
	Status() PrinterStatus
}

type PrinterStatus struct {
	Up        bool      `json:"up"`
	Mode      string    `json:"mode"` // "network:host:port" | "usb:/dev/usb/lp0" | "unknown"
	LastError string    `json:"last_error,omitempty"`
	LastOKAt  time.Time `json:"last_ok_at,omitempty"`
}

// Renderer is implemented by internal/render. Pure functions, no I/O besides embedded fonts.
type Renderer interface {
	// PrepareImage decodes UNTRUSTED bytes -> 1-bit raster at printer width. Error text is user-safe.
	PrepareImage(raw []byte) (*Bitmap, error)
	ASCII(b *Bitmap, columns int) string
	// PreviewReceipt renders what a message receipt would look like (PNG bytes), for the admin UI.
	PreviewReceipt(m Message, s Settings) ([]byte, error)
	PreviewBitmap(b *Bitmap) ([]byte, error)
}

// Gray is a helper alias used by render tests.
type Gray = image.Gray
