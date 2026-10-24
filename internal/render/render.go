// Package render turns messages, uploaded images and ASCII art into ESC/POS
// documents and admin-UI previews. It is pure computation: the only data it
// reads are the embedded fonts.
package render

import (
	"errors"
	"time"
	_ "time/tzdata" // the display zone must resolve even in a scratch image

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
)

// DefaultDisplayTZ is used when Config.DisplayTZ is nil.
const DefaultDisplayTZ = "America/Los_Angeles"

// Config is the env-derived static configuration of the renderer. Zero values
// select the Python defaults.
type Config struct {
	WidthPx        int // PRINTER_WIDTH_PX (576)
	MaxImageBytes  int // MAX_IMAGE_BYTES (10 MiB)
	MaxImageHeight int // MAX_IMAGE_HEIGHT (1200)
	MaxImagePixels int // MAX_IMAGE_PIXELS (24M)
	LineWidth      int // default line_width when no setting is present (32)
	DisplayTZ      *time.Location
}

type renderer struct {
	cfg    Config
	layout Layout
	lim    imageLimits
	tz     *time.Location
}

// New returns the domain.Renderer implementation.
func New(cfg Config) domain.Renderer {
	lim := imageLimits{WidthPx: cfg.WidthPx, MaxBytes: cfg.MaxImageBytes, MaxHeight: cfg.MaxImageHeight, MaxPixels: cfg.MaxImagePixels}.withDefaults()
	tz := cfg.DisplayTZ
	if tz == nil {
		if l, err := time.LoadLocation(DefaultDisplayTZ); err == nil {
			tz = l
		} else {
			tz = time.UTC
		}
	}
	l := Layout{
		WidthPx:        lim.WidthPx,
		LineWidth:      cfg.LineWidth,
		MaxImageBytes:  lim.MaxBytes,
		MaxImageHeight: lim.MaxHeight,
		MaxImagePixels: lim.MaxPixels,
	}.withDefaults()
	return &renderer{cfg: cfg, layout: l, lim: lim, tz: tz}
}

// PrepareImage decodes UNTRUSTED bytes into a printer-width 1-bit raster.
// Errors are *ImageError values whose text is safe to show to users.
func (r *renderer) PrepareImage(raw []byte) (*domain.Bitmap, error) {
	return prepareImage(raw, r.lim)
}

// ASCII converts a raster to ASCII art of the given width in characters.
func (r *renderer) ASCII(b *domain.Bitmap, columns int) string {
	return bitmapToASCII(b, columns)
}

// PreviewReceipt renders what a message receipt would look like as a PNG.
func (r *renderer) PreviewReceipt(m domain.Message, s domain.Settings) ([]byte, error) {
	if m.ReceivedAt == "" && (m.BrowserTime == nil || *m.BrowserTime == "") {
		m.ReceivedAt = time.Now().UTC().Format(domain.TimeLayout)
	}
	ops, err := buildOps(domain.Job{Kind: domain.JobMessage, MsgID: m.ID, Msg: &m}, s, r.layout, r.tz)
	if err != nil {
		return nil, err
	}
	g, err := composePreview(ops, r.layout.WidthPx)
	if err != nil {
		return nil, err
	}
	return encodePNG(g)
}

// PreviewBitmap renders a 1-bit raster on simulated paper as a PNG.
func (r *renderer) PreviewBitmap(b *domain.Bitmap) ([]byte, error) {
	if b == nil || b.W <= 0 || b.H <= 0 || len(b.Pix) < b.W*b.H {
		return nil, errors.New("render: empty bitmap")
	}
	return encodePNG(bitmapGray(b))
}
