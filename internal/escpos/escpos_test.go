package escpos

import (
	"bytes"
	"testing"
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
)

func bitmap(w, h int, set func(x, y int) bool) *domain.Bitmap {
	bm := &domain.Bitmap{W: w, H: h, Pix: make([]bool, w*h)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			bm.Pix[y*w+x] = set(x, y)
		}
	}
	return bm
}

func eq(t *testing.T, name string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Fatalf("%s:\n got  % x\n want % x", name, got, want)
	}
}

func TestSimpleCommands(t *testing.T) {
	cases := []struct {
		name string
		fn   func(b *Builder)
		want []byte
	}{
		{"init", func(b *Builder) { b.Init() }, []byte{0x1b, 0x40}},
		{"align left", func(b *Builder) { b.Align(Left) }, []byte{0x1b, 0x61, 0}},
		{"align center", func(b *Builder) { b.Align(Center) }, []byte{0x1b, 0x61, 1}},
		{"align right", func(b *Builder) { b.Align(Right) }, []byte{0x1b, 0x61, 2}},
		{"align invalid", func(b *Builder) { b.Align(Align(9)) }, []byte{0x1b, 0x61, 0}},
		{"bold on", func(b *Builder) { b.Bold(true) }, []byte{0x1b, 0x45, 1}},
		{"bold off", func(b *Builder) { b.Bold(false) }, []byte{0x1b, 0x45, 0}},
		{"font b", func(b *Builder) { b.FontB(true) }, []byte{0x1b, 0x4d, 1}},
		{"font a", func(b *Builder) { b.FontB(false) }, []byte{0x1b, 0x4d, 0}},
		{"spacing 18", func(b *Builder) { b.LineSpacing(18) }, []byte{0x1b, 0x33, 18}},
		{"spacing default", func(b *Builder) { b.LineSpacing(0) }, []byte{0x1b, 0x32}},
		{"spacing clamp", func(b *Builder) { b.LineSpacing(999) }, []byte{0x1b, 0x33, 255}},
		{"feed 3", func(b *Builder) { b.Feed(3) }, []byte{0x0a, 0x0a, 0x0a}},
		{"feed 0", func(b *Builder) { b.Feed(0) }, nil},
		{"text", func(b *Builder) { b.Text("Hi\n") }, []byte("Hi\n")},
		{"text sanitised", func(b *Builder) { b.Text("a\x1bb\u00e9c\x7f") }, []byte("ab?c")},
		{"cut", func(b *Builder) { b.Cut() }, []byte{0x1b, 0x64, 6, 0x1d, 0x56, 0}},
		{"cut partial", func(b *Builder) { b.CutPartial() }, []byte{0x1b, 0x64, 6, 0x1d, 0x56, 1}},
	}
	for _, c := range cases {
		b := NewBuilder(Options{})
		c.fn(b)
		eq(t, c.name, b.Doc().Bytes(), c.want)
	}
}

func TestChaining(t *testing.T) {
	d := NewBuilder(Options{}).Init().Align(Center).Bold(true).Text("T\n").Bold(false).Cut().Doc()
	if len(d) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(d))
	}
	eq(t, "chain", d[0].Data, []byte{0x1b, 0x40, 0x1b, 0x61, 1, 0x1b, 0x45, 1, 'T', '\n', 0x1b, 0x45, 0, 0x1b, 0x64, 6, 0x1d, 0x56, 0})
}

func TestEmptyDoc(t *testing.T) {
	if d := NewBuilder(Options{}).Doc(); len(d) != 0 {
		t.Fatalf("want empty doc, got %v", d)
	}
	if d := NewBuilder(Options{}).Raster(nil).Raster(&domain.Bitmap{}).Doc(); len(d) != 0 {
		t.Fatalf("nil/empty raster must be a no-op, got %v", d)
	}
}

// 10x2 image: top row all black, bottom row only x=0 black.
func tinyBitmap() *domain.Bitmap {
	return bitmap(10, 2, func(x, y int) bool { return y == 0 || x == 0 })
}

func TestRasterGSv0(t *testing.T) {
	d := NewBuilder(Options{ImageImpl: ImplBitImageRaster}).Raster(tinyBitmap()).Doc()
	if len(d) != 1 || d[0].PauseAfter != 0 {
		t.Fatalf("want single unpaused chunk, got %+v", d)
	}
	// GS v 0 m=0 xL=2 xH=0 yL=2 yH=0, row0 = FF C0, row1 = 80 00
	eq(t, "gs v 0", d[0].Data, []byte{0x1d, 'v', '0', 0, 2, 0, 2, 0, 0xff, 0xc0, 0x80, 0x00})
}

func TestRasterColumnMode33(t *testing.T) {
	d := NewBuilder(Options{}).Raster(tinyBitmap()).Doc() // default impl = bitImageColumn
	if len(d) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(d))
	}
	want := []byte{0x1b, '3', 16, 0x1b, '*', 33, 10, 0}
	// column 0: rows 0 and 1 set -> top byte 0xC0; column 1..9: row 0 only -> 0x80
	want = append(want, 0xc0, 0, 0)
	for x := 1; x < 10; x++ {
		want = append(want, 0x80, 0, 0)
	}
	want = append(want, '\n') // trailing LF per stripe
	want = append(want, 0x1b, '2')
	eq(t, "esc * 33", d[0].Data, want)
}

func TestColumnModeStripesAndPadding(t *testing.T) {
	// 1 wide, 25 tall, every pixel black -> 2 stripes; second has 1 row set (0x80,0,0).
	bm := bitmap(1, 25, func(x, y int) bool { return true })
	got := NewBuilder(Options{}).Raster(bm).Doc().Bytes()
	want := []byte{0x1b, '3', 16,
		0x1b, '*', 33, 1, 0, 0xff, 0xff, 0xff, '\n',
		0x1b, '*', 33, 1, 0, 0x80, 0x00, 0x00, '\n',
		0x1b, '2'}
	eq(t, "stripes", got, want)
}

func TestRasterGraphics(t *testing.T) {
	d := NewBuilder(Options{ImageImpl: ImplGraphics}).Raster(tinyBitmap()).Doc()
	if len(d) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(d))
	}
	// data = 4 bytes; p = 4 + 10 = 14
	want := []byte{0x1d, '(', 'L', 14, 0, 48, 112, 48, 1, 1, 49, 10, 0, 2, 0, 0xff, 0xc0, 0x80, 0x00,
		0x1d, '(', 'L', 2, 0, 48, 50}
	eq(t, "graphics", d[0].Data, want)
}

func TestGraphicsFragmentsFitLengthField(t *testing.T) {
	bm := bitmap(576, 2000, func(x, y int) bool { return x == y%576 })
	data := encodeGraphics(bm.W, bm.H, bm.Pix)
	// walk the commands and verify every declared length is consistent
	p := 0
	prints, stores := 0, 0
	for p < len(data) {
		if !bytes.HasPrefix(data[p:], []byte{0x1d, '(', 'L'}) {
			t.Fatalf("bad header at %d", p)
		}
		n := int(data[p+3]) | int(data[p+4])<<8
		switch data[p+6] {
		case 112:
			stores++
		case 50:
			prints++
		default:
			t.Fatalf("unexpected fn %d", data[p+6])
		}
		p += 5 + n
	}
	if p != len(data) {
		t.Fatalf("length mismatch %d != %d", p, len(data))
	}
	if stores != prints || stores < 3 {
		t.Fatalf("stores=%d prints=%d", stores, prints)
	}
}

func TestBandedSplittingAndPausing(t *testing.T) {
	// 8 wide, 300 tall; band 128 snaps to 120 -> bands 120,120,60
	bm := bitmap(8, 300, func(x, y int) bool { return (x+y)%2 == 0 })
	pause := 200 * time.Millisecond
	b := NewBuilder(Options{Banded: true, BandPx: 128, BandPause: pause, ImageImpl: ImplBitImageRaster})
	d := b.Text("x\n").Raster(bm).Feed(1).Cut().Doc()
	// chunks: text, 3 bands, trailing feed+cut
	if len(d) != 5 {
		t.Fatalf("want 5 chunks, got %d", len(d))
	}
	eq(t, "text chunk", d[0].Data, []byte("x\n"))
	if d[0].PauseAfter != 0 {
		t.Fatal("text chunk must not pause")
	}
	heights := []int{120, 120, 60}
	for i, h := range heights {
		c := d[1+i]
		want := encodeRaster(8, h, bm.Pix[i*120*8:(i*120+h)*8])
		eq(t, "band", c.Data, want)
		if c.PauseAfter != pause {
			t.Fatalf("band %d pause = %v, want %v (EVERY band incl. last)", i, c.PauseAfter, pause)
		}
	}
	eq(t, "tail", d[4].Data, append([]byte{'\n'}, 0x1b, 'd', 6, 0x1d, 'V', 0))
	if d[4].PauseAfter != 0 {
		t.Fatal("tail must not pause")
	}
}

func TestBandSnapping(t *testing.T) {
	cases := []struct{ in, want int }{{128, 120}, {24, 24}, {5, 24}, {0, 120}, {-1, 120}, {250, 240}, {48, 48}}
	for _, c := range cases {
		if got := snapBand(c.in); got != c.want {
			t.Errorf("snapBand(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestBandedColumnModeEachBandOwnCommand(t *testing.T) {
	bm := bitmap(4, 48, func(x, y int) bool { return true })
	d := NewBuilder(Options{Banded: true, BandPx: 24, BandPause: time.Millisecond}).Raster(bm).Doc()
	if len(d) != 2 {
		t.Fatalf("want 2 bands, got %d", len(d))
	}
	for i, c := range d {
		if !bytes.HasPrefix(c.Data, []byte{0x1b, '3', 16, 0x1b, '*', 33, 4, 0}) || !bytes.HasSuffix(c.Data, []byte{'\n', 0x1b, '2'}) {
			t.Fatalf("band %d not a self-contained column command: % x", i, c.Data)
		}
		if c.PauseAfter != time.Millisecond {
			t.Fatalf("band %d missing pause", i)
		}
	}
}

func TestUnbandedIsSingleCommandNoPause(t *testing.T) {
	bm := bitmap(8, 500, func(x, y int) bool { return true })
	d := NewBuilder(Options{BandPx: 24, BandPause: time.Second, ImageImpl: ImplBitImageRaster}).Raster(bm).Doc()
	if len(d) != 1 || d[0].PauseAfter != 0 {
		t.Fatalf("want one unpaused chunk, got %d chunks", len(d))
	}
	eq(t, "single", d[0].Data, encodeRaster(8, 500, bm.Pix))
}

func TestWidthCrop(t *testing.T) {
	bm := bitmap(16, 1, func(x, y int) bool { return true })
	got := NewBuilder(Options{WidthPx: 8, ImageImpl: ImplBitImageRaster}).Raster(bm).Doc().Bytes()
	eq(t, "crop", got, []byte{0x1d, 'v', '0', 0, 1, 0, 1, 0, 0xff})
}

func TestDocHelpers(t *testing.T) {
	d := Doc{{Data: []byte{1}, PauseAfter: time.Second}, {Data: []byte{2, 3}, PauseAfter: time.Second}}
	eq(t, "bytes", d.Bytes(), []byte{1, 2, 3})
	if d.Pause() != 2*time.Second {
		t.Fatal("pause sum")
	}
}
