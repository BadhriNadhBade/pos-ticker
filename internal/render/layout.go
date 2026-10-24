package render

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
	"github.com/badhrinadhbade/pos-ticker/internal/escpos"
)

// Layout carries the printer geometry and pacing used to build a job. The
// zero value of every field falls back to the Python defaults, so a partially
// filled Layout is valid.
type Layout struct {
	WidthPx          int
	LineWidth        int // default for the line_width setting (32)
	ASCIIColumns     int
	ASCIILineSpacing int
	Banded           bool
	BandPx           int
	BandPause        time.Duration
	ImageImpl        string

	// Optional image limits for stored message images (0 = defaults).
	MaxImageBytes  int
	MaxImageHeight int
	MaxImagePixels int
}

func (l Layout) withDefaults() Layout {
	if l.WidthPx <= 0 {
		l.WidthPx = DefaultWidthPx
	}
	if l.LineWidth <= 0 {
		l.LineWidth = 32
	}
	if l.ASCIIColumns <= 0 {
		l.ASCIIColumns = (l.WidthPx - 12) / 9
		if l.ASCIIColumns < 16 {
			l.ASCIIColumns = 16
		}
	}
	if l.ASCIILineSpacing <= 0 {
		l.ASCIILineSpacing = 18
	}
	if l.BandPx <= 0 {
		l.BandPx = 128
	}
	// Snap to a multiple of 24 (ESC * high-density stripe) so bands tile.
	l.BandPx = (l.BandPx / 24) * 24
	if l.BandPx < 24 {
		l.BandPx = 24
	}
	if l.ImageImpl == "" {
		l.ImageImpl = "bitImageColumn"
	}
	return l
}

func (l Layout) limits() imageLimits {
	return imageLimits{WidthPx: l.WidthPx, MaxBytes: l.MaxImageBytes, MaxHeight: l.MaxImageHeight, MaxPixels: l.MaxImagePixels}
}

// ---- layout ops: the single description of a receipt, shared by ESC/POS and preview ----

type opKind uint8

const (
	opInit opKind = iota
	opAlign
	opBold
	opFontB
	opSpacing
	opText
	opFeed
	opRaster
	opCut
)

type op struct {
	k     opKind
	align escpos.Align
	on    bool
	n     int
	s     string
	bm    *domain.Bitmap
}

type opList []op

func (o *opList) init()                { *o = append(*o, op{k: opInit}) }
func (o *opList) align(a escpos.Align) { *o = append(*o, op{k: opAlign, align: a}) }
func (o *opList) bold(on bool)         { *o = append(*o, op{k: opBold, on: on}) }
func (o *opList) fontB(on bool)        { *o = append(*o, op{k: opFontB, on: on}) }
func (o *opList) spacing(n int)        { *o = append(*o, op{k: opSpacing, n: n}) }
func (o *opList) text(s string)        { *o = append(*o, op{k: opText, s: s}) }
func (o *opList) feed(n int)           { *o = append(*o, op{k: opFeed, n: n}) }
func (o *opList) raster(b *domain.Bitmap) {
	*o = append(*o, op{k: opRaster, bm: b})
}
func (o *opList) cut() { *o = append(*o, op{k: opCut}) }

// emit replays ops onto an escpos.Builder.
func (o opList) emit(b *escpos.Builder) {
	for _, x := range o {
		switch x.k {
		case opInit:
			b.Init()
		case opAlign:
			b.Align(x.align)
		case opBold:
			b.Bold(x.on)
		case opFontB:
			b.FontB(x.on)
		case opSpacing:
			b.LineSpacing(x.n)
		case opText:
			b.Text(x.s)
		case opFeed:
			b.Feed(x.n)
		case opRaster:
			b.Raster(x.bm)
		case opCut:
			b.Cut()
		}
	}
}

// ---- settings access ----

func setStr(s domain.Settings, key, def string) string {
	if s == nil {
		return def
	}
	return s.Get(key)
}

func setInt(s domain.Settings, key string, def int) int {
	if s == nil {
		return def
	}
	return s.Int(key, def)
}

func settingTrue(s domain.Settings, key string) bool {
	return strings.ToLower(setStr(s, key, "true")) != "false"
}

// ---- BuildJob ----

// BuildJob renders a print job into an ESC/POS document. s supplies the receipt
// settings (title, footer, fonts, line_width, show_*), l the printer geometry
// and pacing, tz the display time zone for UTC timestamps (nil = UTC).
func BuildJob(j domain.Job, s domain.Settings, l Layout, tz *time.Location) (escpos.Doc, error) {
	l = l.withDefaults()
	ops, err := buildOps(j, s, l, tz)
	if err != nil {
		return nil, err
	}
	b := escpos.NewBuilder(escpos.Options{
		WidthPx:   l.WidthPx,
		Banded:    l.Banded,
		BandPx:    l.BandPx,
		BandPause: l.BandPause,
		ImageImpl: l.ImageImpl,
	})
	ops.emit(b)
	return b.Doc(), nil
}

func buildOps(j domain.Job, s domain.Settings, l Layout, tz *time.Location) (opList, error) {
	switch j.Kind {
	case domain.JobMessage:
		if j.Msg == nil {
			return nil, errors.New("render: message job without message")
		}
		return messageOps(j.Msg, j.MsgID, s, l, tz)
	case domain.JobImage:
		if j.Raster == nil || j.Raster.W <= 0 || j.Raster.H <= 0 || len(j.Raster.Pix) < j.Raster.W*j.Raster.H {
			return nil, errors.New("render: image job without raster")
		}
		var o opList
		o.init()
		o.align(escpos.Center)
		o.raster(j.Raster)
		o.align(escpos.Left)
		o.feed(1)
		o.cut()
		return o, nil
	case domain.JobASCII:
		var o opList
		o.init()
		o.align(escpos.Left)
		o.fontB(true)
		o.spacing(l.ASCIILineSpacing)
		for _, line := range strings.Split(asciiSafe(j.Text, true), "\n") {
			o.text(line + "\n")
		}
		o.spacing(0) // restore printer default
		o.fontB(false)
		o.feed(1)
		o.cut()
		return o, nil
	}
	return nil, fmt.Errorf("render: unknown job kind %q", j.Kind)
}

func clampLW(lw int) int {
	if lw < 10 {
		return 10
	}
	if lw > 255 {
		return 255
	}
	return lw
}

// messageOps ports _do_print.
func messageOps(m *domain.Message, jobID int64, s domain.Settings, l Layout, tz *time.Location) (opList, error) {
	lw := clampLW(setInt(s, "line_width", l.LineWidth))

	title := strings.ToUpper(asciiSafe(strings.TrimSpace(setStr(s, "receipt_title", "")), false))
	if title == "" {
		title = "NEW MESSAGE"
	}
	footer := strings.ToUpper(asciiSafe(strings.TrimSpace(setStr(s, "receipt_footer", "")), false))
	showTS := settingTrue(s, "receipt_show_timestamp")
	showEmail := settingTrue(s, "receipt_show_email")
	showID := settingTrue(s, "receipt_show_id")

	ts := ""
	if m.BrowserTime != nil && *m.BrowserTime != "" {
		ts = *m.BrowserTime
	} else {
		ts = utcToDisplay(m.ReceivedAt, tz)
	}
	tsLine := timestampLine(asciiSafe(ts, false))

	id := m.ID
	if id == 0 {
		id = jobID
	}

	thin := strings.Repeat("-", lw)
	name := asciiSafe(m.Name, false)
	email := asciiSafe(m.Email, false)

	var o opList
	o.init()
	// Title (centred, bold)
	o.align(escpos.Center)
	o.bold(true)
	o.text(truncateToWidth(title, lw) + "\n")
	o.bold(false)

	// Timestamp subtitle
	if showTS {
		o.align(escpos.Center)
		o.text(truncateToWidth(tsLine, lw) + "\n")
	}
	o.align(escpos.Left)
	o.feed(1)

	o.text(thin + "\n")
	o.text(kvRow("FROM", name, lw))
	if showEmail {
		o.text(kvRow("EMAIL", email, lw))
	}

	// Message block
	o.text(thin + "\n")
	if hasNonASCII(m.Body) {
		fontName := setStr(s, "receipt_font", FontReceipt)
		size := setInt(s, "receipt_font_size", 22)
		if size < 8 {
			size = 8
		} else if size > 96 {
			size = 96
		}
		bm, err := renderTextBitmap(m.Body, fontName, size, l.WidthPx)
		if err != nil {
			return nil, fmt.Errorf("render message bitmap: %w", err)
		}
		o.align(escpos.Left)
		o.raster(bm)
	} else {
		o.bold(true)
		for _, line := range limitBodyLines(wrapText(asciiSafe(m.Body, true), lw)) {
			o.text(line + "\n")
		}
		o.bold(false)
	}

	// Attached image (stored bytes are the ORIGINAL upload; rasterised here)
	if len(m.Image) > 0 {
		bm, err := prepareImage(m.Image, l.limits())
		if err != nil {
			slog.Warn("stored image unprintable", "id", id, "err", err)
		} else {
			o.feed(1)
			o.align(escpos.Center)
			o.raster(bm)
			o.align(escpos.Left)
		}
	}

	// Meta
	o.text(thin + "\n")
	if showID {
		o.align(escpos.Center)
		o.text("#" + strconv.FormatInt(id, 10) + "\n")
		o.align(escpos.Left)
	}

	// Footer (only if configured)
	if footer != "" {
		o.feed(1)
		o.align(escpos.Center)
		o.text(truncateToWidth(footer, lw) + "\n")
		o.align(escpos.Left)
	}
	o.feed(1)
	o.cut()
	return o, nil
}
