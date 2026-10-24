package render

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
)

func testRenderer() *renderer {
	return New(Config{}).(*renderer)
}

func encPNG(t *testing.T, im image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func gradient(w, h int) *image.RGBA {
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(x * 255 / (w - 1))
			im.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return im
}

func blackFraction(b *domain.Bitmap) float64 {
	n := 0
	for _, p := range b.Pix {
		if p {
			n++
		}
	}
	return float64(n) / float64(len(b.Pix))
}

func TestPrepareImagePNGResizesToWidth(t *testing.T) {
	r := testRenderer()
	bm, err := r.PrepareImage(encPNG(t, gradient(200, 100)))
	if err != nil {
		t.Fatal(err)
	}
	if bm.W != 576 {
		t.Errorf("W=%d want 576", bm.W)
	}
	if bm.H != 288 { // round(100*576/200)
		t.Errorf("H=%d want 288", bm.H)
	}
	if len(bm.Pix) != bm.W*bm.H {
		t.Errorf("pix len %d", len(bm.Pix))
	}
	// Left edge is dark, right edge is light after contrast + dithering.
	left, right := 0, 0
	for y := 0; y < bm.H; y++ {
		for x := 0; x < 40; x++ {
			if bm.Pix[y*bm.W+x] {
				left++
			}
			if bm.Pix[y*bm.W+bm.W-1-x] {
				right++
			}
		}
	}
	if left <= right*4 {
		t.Errorf("gradient direction lost: left=%d right=%d", left, right)
	}
	// Mid-gray dithers to roughly half ink.
	mid := image.NewRGBA(image.Rect(0, 0, 576, 50))
	for i := range mid.Pix {
		mid.Pix[i] = 128
		if i%4 == 3 {
			mid.Pix[i] = 255
		}
	}
	bm2, err := r.PrepareImage(encPNG(t, mid))
	if err != nil {
		t.Fatal(err)
	}
	if f := blackFraction(bm2); f < 0.4 || f > 0.6 {
		t.Errorf("mid gray ink fraction %.2f", f)
	}
}

func TestPrepareImageJPEGAndGIF(t *testing.T) {
	r := testRenderer()
	var jb bytes.Buffer
	if err := jpeg.Encode(&jb, gradient(64, 64), nil); err != nil {
		t.Fatal(err)
	}
	bm, err := r.PrepareImage(jb.Bytes())
	if err != nil || bm.W != 576 || bm.H != 576 {
		t.Fatalf("jpeg: %v %+v", err, bm)
	}

	pal := image.NewPaletted(image.Rect(0, 0, 30, 20), color.Palette{color.Black, color.White})
	for y := 0; y < 20; y++ {
		for x := 0; x < 30; x++ {
			pal.SetColorIndex(x, y, uint8((x/5+y/5)%2))
		}
	}
	var gb bytes.Buffer
	if err := gif.Encode(&gb, pal, nil); err != nil {
		t.Fatal(err)
	}
	bm, err = r.PrepareImage(gb.Bytes())
	if err != nil || bm.W != 576 || bm.H != 384 {
		t.Fatalf("gif: %v %+v", err, bm)
	}
	if f := blackFraction(bm); f < 0.3 || f > 0.7 {
		t.Errorf("checker ink fraction %.2f", f)
	}
}

func TestPrepareImageWebP(t *testing.T) {
	// 1x1 lossless WebP.
	raw, err := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
	if err != nil {
		t.Fatal(err)
	}
	bm, err := testRenderer().PrepareImage(raw)
	if err != nil {
		t.Fatalf("webp: %v", err)
	}
	if bm.W != 576 || bm.H != 576 {
		t.Errorf("webp size %dx%d", bm.W, bm.H)
	}
}

func TestPrepareImageAlphaFlattenOnWhite(t *testing.T) {
	// Fully transparent black must print as white paper, not a black slab.
	tr := image.NewNRGBA(image.Rect(0, 0, 100, 40)) // zero value = transparent black
	bm, err := testRenderer().PrepareImage(encPNG(t, tr))
	if err != nil {
		t.Fatal(err)
	}
	if f := blackFraction(bm); f != 0 {
		t.Errorf("transparent image has ink fraction %.3f", f)
	}
	// Opaque black stays black.
	bl := image.NewNRGBA(image.Rect(0, 0, 100, 40))
	for i := 0; i < len(bl.Pix); i += 4 {
		bl.Pix[i+3] = 255
	}
	bm, err = testRenderer().PrepareImage(encPNG(t, bl))
	if err != nil {
		t.Fatal(err)
	}
	if f := blackFraction(bm); f != 1 {
		t.Errorf("opaque black ink fraction %.3f", f)
	}
	// 50% black over white = mid gray -> half ink.
	half := image.NewNRGBA(image.Rect(0, 0, 100, 40))
	for i := 0; i < len(half.Pix); i += 4 {
		half.Pix[i+3] = 128
	}
	bm, err = testRenderer().PrepareImage(encPNG(t, half))
	if err != nil {
		t.Fatal(err)
	}
	if f := blackFraction(bm); f < 0.3 || f > 0.7 {
		t.Errorf("half alpha ink fraction %.3f", f)
	}
}

func TestPrepareImageCropsToMaxHeight(t *testing.T) {
	r := New(Config{MaxImageHeight: 100}).(*renderer)
	bm, err := r.PrepareImage(encPNG(t, gradient(50, 400)))
	if err != nil {
		t.Fatal(err)
	}
	if bm.W != 576 || bm.H != 100 {
		t.Errorf("got %dx%d want 576x100", bm.W, bm.H)
	}
	// Extreme aspect ratio must not allocate the full intermediate.
	tall := image.NewGray(image.Rect(0, 0, 1, 20000))
	bm, err = New(Config{}).PrepareImage(encPNG(t, tall))
	if err != nil || bm.H != 1200 {
		t.Errorf("tall: %v %v", err, bm)
	}
}

// pngHeader builds a signature + IHDR-only PNG (no IDAT) claiming w x h.
func pngHeader(w, h uint32) []byte {
	var b bytes.Buffer
	b.Write([]byte("\x89PNG\r\n\x1a\n"))
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 2 // 8-bit RGB
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], 13)
	b.Write(l[:])
	body := append([]byte("IHDR"), ihdr...)
	b.Write(body)
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], crc32.ChecksumIEEE(body))
	b.Write(c[:])
	return b.Bytes()
}

func TestPrepareImageRejectsPixelBombBeforeDecoding(t *testing.T) {
	// 100000 x 100000 = 10 Gpx declared in a ~33 byte file. If the code tried
	// to decode, it would fail with "unreadable image" (no IDAT) or, worse,
	// allocate 40 GB; the guard must trip first with the dimensions message.
	_, err := testRenderer().PrepareImage(pngHeader(100000, 100000))
	var ie *ImageError
	if !errors.As(err, &ie) || ie.Msg != "image dimensions too large" {
		t.Fatalf("want dimensions error, got %v", err)
	}
	// Just over the limit (5001x4800 = 24.0048M).
	_, err = testRenderer().PrepareImage(pngHeader(5001, 4800))
	if err == nil || err.Error() != "image dimensions too large" {
		t.Errorf("limit+: %v", err)
	}
	// Exactly at the limit passes the guard (and then fails to decode: no IDAT).
	_, err = testRenderer().PrepareImage(pngHeader(6000, 4000))
	if err == nil || err.Error() != "unreadable image" {
		t.Errorf("limit: %v", err)
	}
}

func TestPrepareImageRejectsBadInput(t *testing.T) {
	r := testRenderer()
	good := encPNG(t, gradient(50, 50))
	cases := map[string][]byte{
		"empty":     nil,
		"garbage":   []byte("this is definitely not an image at all"),
		"truncated": good[:len(good)/2],
		"zeros":     make([]byte, 4096),
		"html":      []byte("<html><script>alert(1)</script></html>"),
	}
	for name, raw := range cases {
		bm, err := r.PrepareImage(raw)
		if err == nil || bm != nil {
			t.Errorf("%s: expected error, got %v %v", name, bm, err)
			continue
		}
		var ie *ImageError
		if !errors.As(err, &ie) {
			t.Errorf("%s: error type %T not user-safe", name, err)
		}
		// User-safe: short, no Go decoder internals, no paths.
		if strings.Contains(err.Error(), "png:") || strings.Contains(err.Error(), "image:") || strings.Contains(err.Error(), "unexpected EOF") {
			t.Errorf("%s: leaks decoder text: %q", name, err)
		}
	}
	small := New(Config{MaxImageBytes: 100})
	if _, err := small.PrepareImage(good); err == nil || !strings.HasPrefix(err.Error(), "image too large") {
		t.Errorf("byte cap: %v", err)
	}
}

func TestASCIIRowMath(t *testing.T) {
	// 100x100 all-white bitmap at 62 columns -> rows = round(62*1*0.5) = 31.
	b := &domain.Bitmap{W: 100, H: 100, Pix: make([]bool, 100*100)}
	art := bitmapToASCII(b, 62)
	// All white -> ramp[9] = ' ' -> every line right-trimmed to "".
	lines := strings.Split(art, "\n")
	if len(lines) != 31 {
		t.Errorf("rows=%d want 31", len(lines))
	}
	for _, l := range lines {
		if l != "" {
			t.Fatalf("white should trim to empty, got %q", l)
		}
	}
	// All black -> '@' x columns, no trailing trim.
	for i := range b.Pix {
		b.Pix[i] = true
	}
	lines = strings.Split(bitmapToASCII(b, 40), "\n")
	if len(lines) != 20 {
		t.Errorf("rows=%d want 20", len(lines))
	}
	if lines[0] != strings.Repeat("@", 40) {
		t.Errorf("black row %q", lines[0])
	}
	// Banker's rounding: 1 col x (h/w = 1) -> 0.5 -> rounds to 0 -> min 1 row.
	one := &domain.Bitmap{W: 10, H: 10, Pix: make([]bool, 100)}
	if n := len(strings.Split(bitmapToASCII(one, 1), "\n")); n != 1 {
		t.Errorf("min rows: %d", n)
	}
	// 3 cols square -> 1.5 -> RoundToEven = 2 (Python round(1.5) == 2).
	if n := len(strings.Split(bitmapToASCII(&domain.Bitmap{W: 10, H: 10, Pix: make([]bool, 100)}, 3), "\n")); n != 2 {
		t.Errorf("round(1.5): %d", n)
	}
	// 5 cols square -> 2.5 -> 2 (banker's), not 3.
	if n := len(strings.Split(bitmapToASCII(&domain.Bitmap{W: 10, H: 10, Pix: make([]bool, 100)}, 5), "\n")); n != 2 {
		t.Errorf("round(2.5): %d", n)
	}
	// Half black (left half) -> mid-ramp chars on the left, ' ' trimmed right.
	half := &domain.Bitmap{W: 20, H: 20, Pix: make([]bool, 400)}
	for y := 0; y < 20; y++ {
		for x := 0; x < 10; x++ {
			half.Pix[y*20+x] = true
		}
	}
	l0 := strings.Split(bitmapToASCII(half, 10), "\n")[0]
	if l0 != "@@@@@" {
		t.Errorf("half row %q", l0)
	}
	if bitmapToASCII(nil, 10) != "" {
		t.Error("nil bitmap")
	}
}

func TestRendererASCIIViaInterface(t *testing.T) {
	r := New(Config{})
	bm, err := r.PrepareImage(encPNG(t, gradient(120, 60)))
	if err != nil {
		t.Fatal(err)
	}
	art := r.ASCII(bm, 60)
	if art == "" || len(strings.Split(art, "\n")) != 15 {
		t.Errorf("art rows=%d", len(strings.Split(art, "\n")))
	}
	for _, ch := range art {
		if ch != '\n' && !strings.ContainsRune(asciiRamp, ch) {
			t.Fatalf("char %q outside ramp", ch)
		}
	}
}
