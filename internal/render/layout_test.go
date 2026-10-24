package render

import (
	"bytes"
	"image"
	"image/png"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/image/math/fixed"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
	"github.com/badhrinadhbade/pos-ticker/internal/escpos"
)

func sp(s string) *string { return &s }

func sampleMsg(body string) *domain.Message {
	return &domain.Message{
		ID: 42, Name: "Alice", Email: "alice@example.com", Body: body,
		ReceivedAt: "2025-05-12 21:41:05",
	}
}

func defaultSettings() mapSettings {
	return mapSettings{
		"line_width": "32", "receipt_font": "receipt", "receipt_font_size": "22",
		"receipt_show_timestamp": "true", "receipt_show_email": "true", "receipt_show_id": "true",
	}
}

func la(t *testing.T) *time.Location {
	t.Helper()
	l, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// textOf concatenates all opText strings.
func textOf(ops opList) string {
	var b strings.Builder
	for _, o := range ops {
		if o.k == opText {
			b.WriteString(o.s)
		}
	}
	return b.String()
}

func countKind(ops opList, k opKind) int {
	n := 0
	for _, o := range ops {
		if o.k == k {
			n++
		}
	}
	return n
}

func TestMessageLayoutPlain(t *testing.T) {
	s := defaultSettings()
	s["receipt_footer"] = "thanks!"
	ops, err := buildOps(domain.Job{Kind: domain.JobMessage, Msg: sampleMsg("hello there world")}, s, Layout{}.withDefaults(), la(t))
	if err != nil {
		t.Fatal(err)
	}
	rule := strings.Repeat("-", 32) + "\n"
	want := "NEW MESSAGE\n" +
		"Mon  May 12  .  2:41 PM\n" +
		rule +
		"FROM" + strings.Repeat(" ", 32-4-5) + "Alice\n" +
		"EMAIL" + strings.Repeat(" ", 32-5-17) + "alice@example.com\n" +
		rule +
		"hello there world\n" +
		rule +
		"#42\n" +
		"THANKS!\n"
	if got := textOf(ops); got != want {
		t.Errorf("text mismatch\n got: %q\nwant: %q", got, want)
	}
	if countKind(ops, opRaster) != 0 {
		t.Error("plain ASCII message must not raster")
	}
	if ops[0].k != opInit || ops[len(ops)-1].k != opCut {
		t.Errorf("must start with init and end with cut: %v ... %v", ops[0].k, ops[len(ops)-1].k)
	}
	// Feed(1) immediately before the cut.
	if p := ops[len(ops)-2]; p.k != opFeed || p.n != 1 {
		t.Errorf("expected feed before cut, got %+v", p)
	}
	// Title is centred+bold; message lines are bold.
	if ops[1].k != opAlign || ops[1].align != escpos.Center || ops[2].k != opBold || !ops[2].on {
		t.Errorf("title header ops: %+v %+v", ops[1], ops[2])
	}
}

func TestMessageLayoutOptions(t *testing.T) {
	s := defaultSettings()
	s["receipt_show_timestamp"] = "false"
	s["receipt_show_email"] = "false"
	s["receipt_show_id"] = "FALSE"
	s["receipt_title"] = "  hi there "
	ops, err := buildOps(domain.Job{Kind: domain.JobMessage, Msg: sampleMsg("x")}, s, Layout{}.withDefaults(), la(t))
	if err != nil {
		t.Fatal(err)
	}
	got := textOf(ops)
	for _, bad := range []string{"EMAIL", "#42", "PM", "NEW MESSAGE"} {
		if strings.Contains(got, bad) {
			t.Errorf("unexpected %q in %q", bad, got)
		}
	}
	if !strings.HasPrefix(got, "HI THERE\n") {
		t.Errorf("custom title: %q", got)
	}
	// Browser time wins over UTC conversion.
	m := sampleMsg("x")
	m.BrowserTime = sp("Tue  Jun 3  9:00 AM")
	s["receipt_show_timestamp"] = "true"
	ops, _ = buildOps(domain.Job{Kind: domain.JobMessage, Msg: m}, s, Layout{}.withDefaults(), la(t))
	if !strings.Contains(textOf(ops), "Tue  Jun 3  .  9:00 AM\n") {
		t.Errorf("browser time: %q", textOf(ops))
	}
}

func TestMessageLayoutStripsEscCodes(t *testing.T) {
	m := sampleMsg("hi\x1b@\x1dV\x00 there")
	m.Name = "Eve\x1b!\x01"
	ops, err := buildOps(domain.Job{Kind: domain.JobMessage, Msg: m}, defaultSettings(), Layout{}.withDefaults(), la(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range ops {
		if o.k != opText {
			continue
		}
		for _, r := range o.s {
			if (r < 0x20 && r != '\n') || r >= 0x7f {
				t.Fatalf("control/non-ASCII %U leaked into Text: %q", r, o.s)
			}
		}
	}
}

func TestMessageLayoutEmojiUsesRaster(t *testing.T) {
	m := sampleMsg("hello 😀 wörld")
	ops, err := buildOps(domain.Job{Kind: domain.JobMessage, Msg: m}, defaultSettings(), Layout{}.withDefaults(), la(t))
	if err != nil {
		t.Fatal(err)
	}
	if countKind(ops, opRaster) != 1 {
		t.Fatalf("want exactly 1 raster, got %d", countKind(ops, opRaster))
	}
	var bm *domain.Bitmap
	for _, o := range ops {
		if o.k == opRaster {
			bm = o.bm
		}
	}
	if bm.W != 576 {
		t.Errorf("block width %d want full paper width 576", bm.W)
	}
	if bm.H < 30 || len(bm.Pix) != bm.W*bm.H {
		t.Errorf("block size %dx%d", bm.W, bm.H)
	}
	if blackFraction(bm) == 0 {
		t.Error("emoji block is blank")
	}
	// The message text itself must not also appear as ESC/POS text.
	if strings.Contains(textOf(ops), "hello") {
		t.Errorf("message duplicated as text: %q", textOf(ops))
	}
	// Header/footer rows still present.
	for _, w := range []string{"NEW MESSAGE", "FROM", "#42"} {
		if !strings.Contains(textOf(ops), w) {
			t.Errorf("missing %q", w)
		}
	}
}

func TestMessageLayoutWithImage(t *testing.T) {
	m := sampleMsg("with picture")
	m.Image = encPNG(t, gradient(80, 40))
	ops, err := buildOps(domain.Job{Kind: domain.JobMessage, Msg: m}, defaultSettings(), Layout{}.withDefaults(), la(t))
	if err != nil {
		t.Fatal(err)
	}
	if countKind(ops, opRaster) != 1 {
		t.Fatalf("rasters=%d", countKind(ops, opRaster))
	}
	// Bad stored image is skipped, receipt still prints.
	m.Image = []byte("junk")
	ops, err = buildOps(domain.Job{Kind: domain.JobMessage, Msg: m}, defaultSettings(), Layout{}.withDefaults(), la(t))
	if err != nil || countKind(ops, opRaster) != 0 {
		t.Fatalf("bad image: err=%v rasters=%d", err, countKind(ops, opRaster))
	}
}

func docBytes(d escpos.Doc) []byte {
	var b bytes.Buffer
	for _, c := range d {
		b.Write(c.Data)
	}
	return b.Bytes()
}

// BuildJob goldens are deliberately encoding-agnostic (the escpos package owns the bytes):
// they check that the text lines and a raster payload reach the document and
// that the document ends with the GS V cut.
func TestBuildJobDocPlainAndEmoji(t *testing.T) {
	l := Layout{Banded: true, BandPx: 100, BandPause: time.Millisecond}
	d, err := BuildJob(domain.Job{Kind: domain.JobMessage, Msg: sampleMsg("plain text")}, defaultSettings(), l, la(t))
	if err != nil {
		t.Fatal(err)
	}
	raw := docBytes(d)
	for _, w := range []string{"NEW MESSAGE\n", "plain text\n", "#42\n"} {
		if !bytes.Contains(raw, []byte(w)) {
			t.Errorf("doc missing %q", w)
		}
	}
	if !bytes.Contains(raw[len(raw)-8:], []byte{0x1d, 'V'}) {
		t.Errorf("doc does not end with a cut: % x", raw[len(raw)-8:])
	}
	plainLen := len(raw)

	d, err = BuildJob(domain.Job{Kind: domain.JobMessage, Msg: sampleMsg("emoji 😀 body")}, defaultSettings(), l, la(t))
	if err != nil {
		t.Fatal(err)
	}
	raw = docBytes(d)
	if bytes.Contains(raw, []byte("emoji")) {
		t.Error("emoji body leaked as text")
	}
	// A 576px-wide raster is at least 72 bytes per row; many more than text.
	if len(raw) < 72*30 {
		t.Errorf("emoji doc (%d bytes) has no raster payload (plain was %d)", len(raw), plainLen)
	}
	if len(d) < 2 {
		t.Errorf("expected multiple chunks (text + raster), got %d", len(d))
	}
}

func TestBuildJobImageAndASCII(t *testing.T) {
	bm := &domain.Bitmap{W: 16, H: 8, Pix: make([]bool, 128)}
	d, err := BuildJob(domain.Job{Kind: domain.JobImage, Raster: bm}, nil, Layout{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := docBytes(d)
	if !bytes.HasSuffix(raw, []byte{0x1d, 'V', 66, 0}) && !bytes.Contains(raw[len(raw)-8:], []byte{0x1d, 'V'}) {
		t.Errorf("image job must end with cut")
	}
	ops, _ := buildOps(domain.Job{Kind: domain.JobImage, Raster: bm}, nil, Layout{}.withDefaults(), nil)
	kinds := []opKind{}
	for _, o := range ops {
		kinds = append(kinds, o.k)
	}
	wantK := []opKind{opInit, opAlign, opRaster, opAlign, opFeed, opCut}
	if len(kinds) != len(wantK) {
		t.Fatalf("image ops %v", kinds)
	}
	for i := range wantK {
		if kinds[i] != wantK[i] {
			t.Fatalf("image ops %v want %v", kinds, wantK)
		}
	}

	ops, err = buildOps(domain.Job{Kind: domain.JobASCII, Text: "@@\n##"}, nil, Layout{}.withDefaults(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var sawSpacing18, sawRestore, sawFontB bool
	for _, o := range ops {
		switch {
		case o.k == opSpacing && o.n == 18:
			sawSpacing18 = true
		case o.k == opSpacing && o.n == 0 && sawSpacing18:
			sawRestore = true
		case o.k == opFontB && o.on:
			sawFontB = true
		}
	}
	if !sawSpacing18 || !sawRestore || !sawFontB || textOf(ops) != "@@\n##\n" {
		t.Errorf("ascii ops wrong: spacing=%v restore=%v fontB=%v text=%q", sawSpacing18, sawRestore, sawFontB, textOf(ops))
	}
	if _, err := BuildJob(domain.Job{Kind: domain.JobImage}, nil, Layout{}, nil); err == nil {
		t.Error("image job without raster should error")
	}
	if _, err := BuildJob(domain.Job{Kind: "bogus"}, nil, Layout{}, nil); err == nil {
		t.Error("unknown kind should error")
	}
	if _, err := BuildJob(domain.Job{Kind: domain.JobMessage}, nil, Layout{}, nil); err == nil {
		t.Error("message job without message should error")
	}
}

func TestLayoutDefaults(t *testing.T) {
	l := Layout{BandPx: 130}.withDefaults()
	if l.BandPx != 120 || l.WidthPx != 576 || l.ASCIIColumns != 62 || l.ASCIILineSpacing != 18 || l.ImageImpl != "bitImageColumn" {
		t.Errorf("%+v", l)
	}
	if c := (Layout{WidthPx: 384}).withDefaults().ASCIIColumns; c != 41 {
		t.Errorf("58mm ascii cols %d want 41", c)
	}
}

func TestFontFallbackPicksEmoji(t *testing.T) {
	for _, name := range []string{"receipt", "mono", "sans", "serif", "bogus"} {
		c, err := newFaceChain(name, 22)
		if err != nil {
			t.Fatal(name, err)
		}
		if got := c.pick('A'); got != 0 {
			t.Errorf("%s: 'A' face %d want primary 0", name, got)
		}
		if got := c.pick(0x1F600); got != c.emojiIdx {
			t.Errorf("%s: U+1F600 face %d want emoji %d", name, got, c.emojiIdx)
		}
		if got := c.pick(0x2764); got != c.emojiIdx { // heavy black heart
			t.Errorf("%s: U+2764 face %d want emoji", name, got)
		}
		c.Close()
	}
	// Accented letter missing from Merchant Copy falls back to DejaVu Sans, not emoji.
	c, _ := newFaceChain("receipt", 22)
	defer c.Close()
	if got := c.pick('ö'); got != 1 {
		t.Errorf("ö face %d want sans fallback 1", got)
	}
	if got := c.pick('ж'); got != 1 {
		t.Errorf("cyrillic face %d want 1", got)
	}
	// Joiners / variation selectors are skipped, not drawn.
	for _, r := range []rune{0x200D, 0xFE0F, 0x1F3FD} {
		if _, _, ok := c.glyph(r); ok {
			t.Errorf("%U should be zero-width", r)
		}
	}
	// Unknown font names normalise to the receipt face.
	if normalizeFontName("Comic") != "receipt" || normalizeFontName(" MONO ") != "mono" {
		t.Error("normalizeFontName")
	}
}

func TestWrapPixelsRespectsWidth(t *testing.T) {
	c, err := newFaceChain("sans", 22)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	max := fixed.I(300)
	text := "the quick brown fox jumps over the lazy dog 😀 and keeps running far away " + strings.Repeat("W", 60)
	lines := c.wrapPixels(text, max)
	if len(lines) < 4 {
		t.Fatalf("expected several lines, got %q", lines)
	}
	for _, ln := range lines {
		if w := c.measure(ln); w > max {
			t.Errorf("line %q is %v px > %v", ln, w, max)
		}
	}
	if got := c.wrapPixels("a\n\nb", max); len(got) != 3 || got[1] != "" {
		t.Errorf("blank paragraph lost: %q", got)
	}
}

func TestRenderTextBitmapEmoji(t *testing.T) {
	bm, err := renderTextBitmap("😀", "mono", 22, 576)
	if err != nil {
		t.Fatal(err)
	}
	if bm.H != 22+8 {
		t.Errorf("one line height %d want 30", bm.H)
	}
	// Ink must sit in the left ~40px only.
	left, rest := 0, 0
	for y := 0; y < bm.H; y++ {
		for x := 0; x < bm.W; x++ {
			if bm.Pix[y*bm.W+x] {
				if x < 40 {
					left++
				} else {
					rest++
				}
			}
		}
	}
	if left < 50 || rest != 0 {
		t.Errorf("emoji ink left=%d rest=%d", left, rest)
	}
	// Multi-line wrap grows the block.
	long, err := renderTextBitmap(strings.Repeat("héllo wörld 😀 ", 20), "receipt", 22, 576)
	if err != nil || long.H <= 30*3 {
		t.Errorf("long text H=%v err=%v", long, err)
	}
}

func TestPreviewReceiptValidPNG(t *testing.T) {
	r := New(Config{DisplayTZ: la(t)})
	for _, body := range []string{"plain preview text that wraps around a bit more", "emoji 😀 preview"} {
		b, err := r.PreviewReceipt(*sampleMsg(body), defaultSettings())
		if err != nil {
			t.Fatal(err)
		}
		im, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("not a PNG: %v", err)
		}
		if im.Bounds().Dx() != 576 || im.Bounds().Dy() < 200 {
			t.Errorf("preview size %v", im.Bounds())
		}
		ink := 0
		g := im.(*image.Gray)
		for _, v := range g.Pix {
			if v < 128 {
				ink++
			}
		}
		if ink < 500 {
			t.Errorf("preview nearly blank (%d ink px)", ink)
		}
	}
	// Preview without a received time falls back to "now" and does not fail.
	if _, err := r.PreviewReceipt(domain.Message{Name: "n", Body: "b"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewBitmapValidPNG(t *testing.T) {
	r := New(Config{})
	bm, err := r.PrepareImage(encPNG(t, gradient(60, 30)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.PreviewBitmap(bm)
	if err != nil {
		t.Fatal(err)
	}
	im, err := png.Decode(bytes.NewReader(b))
	if err != nil || im.Bounds().Dx() != bm.W || im.Bounds().Dy() != bm.H {
		t.Fatalf("png: %v %v", err, im)
	}
	if _, err := r.PreviewBitmap(nil); err == nil {
		t.Error("nil bitmap should error")
	}
}

func TestPreviewComposeSharesLayout(t *testing.T) {
	// The preview is composed from the same ops that BuildJob replays, so a
	// message that gains an image grows taller by about the image height.
	r := New(Config{DisplayTZ: la(t)})
	base, _ := r.PreviewReceipt(*sampleMsg("same"), defaultSettings())
	m := *sampleMsg("same")
	m.Image = encPNG(t, gradient(100, 100))
	with, _ := r.PreviewReceipt(m, defaultSettings())
	bi, _ := png.DecodeConfig(bytes.NewReader(base))
	wi, _ := png.DecodeConfig(bytes.NewReader(with))
	if wi.Height < bi.Height+576 {
		t.Errorf("image not in preview: %d vs %d", wi.Height, bi.Height)
	}
}

func TestRendererConcurrentUse(t *testing.T) {
	r := New(Config{DisplayTZ: la(t)})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := "concurrent 😀 résumé"
			if i%2 == 0 {
				body = "plain concurrent"
			}
			if _, err := r.PreviewReceipt(*sampleMsg(body), defaultSettings()); err != nil {
				t.Error(err)
			}
			if _, err := BuildJob(domain.Job{Kind: domain.JobMessage, Msg: sampleMsg(body)}, defaultSettings(), Layout{}, la(t)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
}
