// Package escpos builds ESC/POS byte streams as a sequence of paced chunks.
//
// The command encodings mirror python-escpos 3.x (the reference implementation
// used by printer_server.py) so the Go port produces byte-identical output for
// the same receipt.
package escpos

import (
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
)

// Image implementation names accepted in Options.ImageImpl.
const (
	ImplBitImageColumn = "bitImageColumn" // ESC * mode 33, 24-dot stripes (the default)
	ImplBitImageRaster = "bitImageRaster" // GS v 0
	ImplGraphics       = "graphics"       // GS ( L fn 112 / fn 50
)

// Align is a horizontal justification.
type Align int

const (
	Left   Align = 0
	Center Align = 1
	Right  Align = 2

	AlignLeft   = Left
	AlignCenter = Center
	AlignRight  = Right
)

// DefaultBandPx mirrors IMAGE_BAND_PX in the Python server.
const DefaultBandPx = 128

// Chunk is one write to the transport, followed by an optional pause.
type Chunk struct {
	Data       []byte
	PauseAfter time.Duration
}

// Doc is written to the transport chunk by chunk, in order.
type Doc []Chunk

// Bytes returns all chunk data concatenated (pauses are dropped).
func (d Doc) Bytes() []byte {
	n := 0
	for _, c := range d {
		n += len(c.Data)
	}
	out := make([]byte, 0, n)
	for _, c := range d {
		out = append(out, c.Data...)
	}
	return out
}

// Pause returns the sum of all PauseAfter values.
func (d Doc) Pause() time.Duration {
	var t time.Duration
	for _, c := range d {
		t += c.PauseAfter
	}
	return t
}

// Options configure a Builder.
type Options struct {
	WidthPx   int           // printable width in dots; wider bitmaps are cropped. 0 = no limit
	Banded    bool          // split rasters into paced bands (network printers)
	BandPx    int           // band height, snapped down to a multiple of 24 (min 24); 0 = DefaultBandPx
	BandPause time.Duration // pause after EVERY band, including the last
	ImageImpl string        // ImplBitImageColumn (default) | ImplBitImageRaster | ImplGraphics
}

// Builder accumulates an ESC/POS document.
type Builder struct {
	opts Options
	buf  []byte
	doc  Doc
}

// NewBuilder returns an empty Builder.
func NewBuilder(opts Options) *Builder {
	if opts.ImageImpl == "" {
		opts.ImageImpl = ImplBitImageColumn
	}
	return &Builder{opts: opts}
}

func (b *Builder) raw(p ...byte) *Builder {
	b.buf = append(b.buf, p...)
	return b
}

// flush moves pending bytes into their own chunk.
func (b *Builder) flush() {
	if len(b.buf) == 0 {
		return
	}
	b.doc = append(b.doc, Chunk{Data: b.buf})
	b.buf = nil
}

// Init emits ESC @ (initialize printer).
func (b *Builder) Init() *Builder { return b.raw(0x1b, '@') }

// Align emits ESC a n.
func (b *Builder) Align(a Align) *Builder {
	if a < Left || a > Right {
		a = Left
	}
	return b.raw(0x1b, 'a', byte(a))
}

// Bold emits ESC E n.
func (b *Builder) Bold(on bool) *Builder { return b.raw(0x1b, 'E', boolByte(on)) }

// FontB selects font B (on) or font A (off) with ESC M n.
func (b *Builder) FontB(on bool) *Builder { return b.raw(0x1b, 'M', boolByte(on)) }

// LineSpacing emits ESC 3 n, or ESC 2 (printer default) when dots <= 0.
func (b *Builder) LineSpacing(dots int) *Builder {
	if dots <= 0 {
		return b.raw(0x1b, '2')
	}
	if dots > 255 {
		dots = 255
	}
	return b.raw(0x1b, '3', byte(dots))
}

// Text appends printable ASCII. Control characters other than LF are dropped
// and non-ASCII runes become '?'; the caller is expected to have rendered such
// text as a bitmap instead.
func (b *Builder) Text(s string) *Builder {
	for _, r := range s {
		switch {
		case r == '\n' || (r >= 0x20 && r < 0x7f):
			b.buf = append(b.buf, byte(r))
		case r < 0x20 || r == 0x7f:
			// drop control characters
		default:
			b.buf = append(b.buf, '?')
		}
	}
	return b
}

// Feed emits n line feeds.
func (b *Builder) Feed(n int) *Builder {
	for i := 0; i < n; i++ {
		b.buf = append(b.buf, '\n')
	}
	return b
}

// Cut matches python-escpos' default cut(): feed 6 lines (ESC d 6) then a full
// cut (GS V 0).
func (b *Builder) Cut() *Builder { return b.raw(0x1b, 'd', 6, 0x1d, 'V', 0) }

// CutPartial is Cut with a partial cut (GS V 1), python-escpos mode="PART".
func (b *Builder) CutPartial() *Builder { return b.raw(0x1b, 'd', 6, 0x1d, 'V', 1) }

// Raster appends a bitmap. With Options.Banded it is split into bands, each
// its own chunk with PauseAfter = BandPause (also after the last band);
// otherwise the whole image is a single chunk.
func (b *Builder) Raster(bm *domain.Bitmap) *Builder {
	if bm == nil || bm.W <= 0 || bm.H <= 0 || len(bm.Pix) < bm.W*bm.H {
		return b
	}
	bm = b.fit(bm)
	b.flush()
	if !b.opts.Banded {
		b.doc = append(b.doc, Chunk{Data: b.encode(bm.W, bm.H, bm.Pix)})
		return b
	}
	band := snapBand(b.opts.BandPx)
	for top := 0; top < bm.H; top += band {
		n := band
		if top+n > bm.H {
			n = bm.H - top
		}
		b.doc = append(b.doc, Chunk{
			Data:       b.encode(bm.W, n, bm.Pix[top*bm.W:(top+n)*bm.W]),
			PauseAfter: b.opts.BandPause,
		})
	}
	return b
}

// Doc returns the finished document. The Builder may keep being used.
func (b *Builder) Doc() Doc {
	b.flush()
	out := make(Doc, len(b.doc))
	copy(out, b.doc)
	return out
}

func snapBand(px int) int {
	if px <= 0 {
		px = DefaultBandPx
	}
	if px = (px / 24) * 24; px < 24 {
		px = 24
	}
	return px
}

// fit crops columns beyond WidthPx.
func (b *Builder) fit(bm *domain.Bitmap) *domain.Bitmap {
	w := b.opts.WidthPx
	if w <= 0 || bm.W <= w {
		return bm
	}
	out := &domain.Bitmap{W: w, H: bm.H, Pix: make([]bool, w*bm.H)}
	for y := 0; y < bm.H; y++ {
		copy(out.Pix[y*w:(y+1)*w], bm.Pix[y*bm.W:y*bm.W+w])
	}
	return out
}

func (b *Builder) encode(w, h int, pix []bool) []byte {
	switch b.opts.ImageImpl {
	case ImplBitImageRaster:
		return encodeRaster(w, h, pix)
	case ImplGraphics:
		return encodeGraphics(w, h, pix)
	default:
		return encodeColumn(w, h, pix)
	}
}

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}

func lowHigh(n int) (byte, byte) { return byte(n & 0xff), byte((n >> 8) & 0xff) }

// packRows packs rows MSB-first, ceil(w/8) bytes per row, black = 1.
func packRows(w, h int, pix []bool) []byte {
	wb := (w + 7) / 8
	out := make([]byte, wb*h)
	for y := 0; y < h; y++ {
		row := pix[y*w : (y+1)*w]
		for x, black := range row {
			if black {
				out[y*wb+x/8] |= 0x80 >> uint(x%8)
			}
		}
	}
	return out
}

// encodeRaster is GS v 0 m xL xH yL yH d... (m=0, normal density).
func encodeRaster(w, h int, pix []bool) []byte {
	wb := (w + 7) / 8
	xl, xh := lowHigh(wb)
	yl, yh := lowHigh(h)
	out := []byte{0x1d, 'v', '0', 0, xl, xh, yl, yh}
	return append(out, packRows(w, h, pix)...)
}

// encodeColumn is python-escpos' bitImageColumn: ESC 3 16, then for each
// 24-dot stripe ESC * 33 nL nH <3 bytes per column> LF, then ESC 2. Note the
// LF after EVERY stripe, including the last one, and that ESC 2 resets line
// spacing to the printer default.
func encodeColumn(w, h int, pix []bool) []byte {
	nl, nh := lowHigh(w)
	out := []byte{0x1b, '3', 16}
	for y0 := 0; y0 < h; y0 += 24 {
		out = append(out, 0x1b, '*', 33, nl, nh)
		for x := 0; x < w; x++ {
			for k := 0; k < 3; k++ {
				var v byte
				for j := 0; j < 8; j++ {
					y := y0 + k*8 + j
					if y < h && pix[y*w+x] {
						v |= 0x80 >> uint(j)
					}
				}
				out = append(out, v)
			}
		}
		out = append(out, '\n')
	}
	return append(out, 0x1b, '2')
}

// encodeGraphics is GS ( L: fn 112 stores a fragment in the print buffer,
// fn 50 prints it. Fragments are at most 960 rows (python-escpos default) and
// small enough that the 16-bit length field cannot overflow.
func encodeGraphics(w, h int, pix []bool) []byte {
	wb := (w + 7) / 8
	frag := 960
	if limit := (65535 - 10) / wb; limit < frag {
		frag = limit
	}
	if frag < 1 {
		frag = 1
	}
	var out []byte
	for top := 0; top < h; top += frag {
		n := frag
		if top+n > h {
			n = h - top
		}
		data := packRows(w, n, pix[top*w:(top+n)*w])
		pl, ph := lowHigh(len(data) + 10)
		xl, xh := lowHigh(w)
		yl, yh := lowHigh(n)
		out = append(out, 0x1d, '(', 'L', pl, ph, 48, 112, 48, 1, 1, 49, xl, xh, yl, yh)
		out = append(out, data...)
		out = append(out, 0x1d, '(', 'L', 2, 0, 48, 50)
	}
	return out
}
