package render

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif" // register decoders
	_ "image/jpeg"
	_ "image/png"
	"math"
	"strings"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
)

// Defaults mirror the original Python server.
const (
	DefaultWidthPx        = 576
	DefaultMaxImageBytes  = 10 * 1024 * 1024
	DefaultMaxImageHeight = 1200
	DefaultMaxImagePixels = 24_000_000
)

// ImageError is returned for untrusted-image problems. Its text is safe to show
// to end users (it never contains decoder internals).
type ImageError struct{ Msg string }

func (e *ImageError) Error() string { return e.Msg }

func imgErr(format string, a ...any) error { return &ImageError{Msg: fmt.Sprintf(format, a...)} }

type imageLimits struct {
	WidthPx, MaxBytes, MaxHeight, MaxPixels int
}

func (l imageLimits) withDefaults() imageLimits {
	if l.WidthPx <= 0 {
		l.WidthPx = DefaultWidthPx
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = DefaultMaxImageBytes
	}
	if l.MaxHeight <= 0 {
		l.MaxHeight = DefaultMaxImageHeight
	}
	if l.MaxPixels <= 0 {
		l.MaxPixels = DefaultMaxImagePixels
	}
	return l
}

// prepareImage decodes UNTRUSTED bytes into a 1-bit raster sized for the
// printer: size cap -> DecodeConfig pixel guard (before any pixel allocation)
// -> decode -> flatten alpha on white -> contrast x1.4 -> resize to width ->
// crop to the paper guard -> Floyd-Steinberg.
func prepareImage(raw []byte, lim imageLimits) (bm *domain.Bitmap, err error) {
	lim = lim.withDefaults()
	if len(raw) == 0 {
		return nil, imgErr("empty image")
	}
	if len(raw) > lim.MaxBytes {
		return nil, imgErr("image too large (max %d bytes)", lim.MaxBytes)
	}
	// Decoders are robust but the input is hostile: never let a panic escape.
	defer func() {
		if r := recover(); r != nil {
			bm, err = nil, imgErr("unreadable image")
		}
	}()

	cfg, _, cerr := image.DecodeConfig(bytes.NewReader(raw))
	if cerr != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, imgErr("unreadable image")
	}
	if int64(cfg.Width)*int64(cfg.Height) > int64(lim.MaxPixels) {
		return nil, imgErr("image dimensions too large")
	}
	src, _, derr := image.Decode(bytes.NewReader(raw))
	if derr != nil {
		return nil, imgErr("unreadable image")
	}
	b := src.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 || int64(b.Dx())*int64(b.Dy()) > int64(lim.MaxPixels) {
		return nil, imgErr("image dimensions too large")
	}

	flat := flattenOnWhite(src)
	applyContrast(flat, 1.4)
	scaled := scaleToWidth(flat, lim.WidthPx, lim.MaxHeight)
	return ditherFS(scaled), nil
}

// flattenOnWhite composites src over opaque white into a fresh origin-based RGBA.
func flattenOnWhite(src image.Image) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for i := 0; i < len(dst.Pix); i++ {
		dst.Pix[i] = 0xff
	}
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	return dst
}

// lumaL24 is Pillow's RGB -> L conversion (ITU-R 601-2).
func lumaL24(r, g, b uint8) int {
	return (int(r)*19595 + int(g)*38470 + int(b)*7471 + 0x8000) >> 16
}

// applyContrast mimics PIL ImageEnhance.Contrast: blend towards the integer mean
// gray of the image by factor. Operates in place on an opaque RGBA.
func applyContrast(img *image.RGBA, factor float64) {
	pix := img.Pix
	n := len(pix) / 4
	if n == 0 {
		return
	}
	var sum uint64
	for i := 0; i < len(pix); i += 4 {
		sum += uint64(lumaL24(pix[i], pix[i+1], pix[i+2]))
	}
	mean := int(float64(sum)/float64(n) + 0.5)
	var lut [256]uint8
	for c := 0; c < 256; c++ {
		v := float64(mean) + factor*float64(c-mean)
		switch {
		case v < 0:
			v = 0
		case v > 255:
			v = 255
		}
		lut[c] = uint8(v)
	}
	for i := 0; i < len(pix); i += 4 {
		pix[i] = lut[pix[i]]
		pix[i+1] = lut[pix[i+1]]
		pix[i+2] = lut[pix[i+2]]
	}
}

// scaleToWidth resizes to exactly w pixels wide (CatmullRom) and keeps at most
// maxH rows. Only the source rows that can survive the crop are scaled, so a
// pathological 1 x 24M image cannot allocate a gigantic intermediate.
func scaleToWidth(src *image.RGBA, w, maxH int) *image.RGBA {
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	if sw == w && sh <= maxH {
		return src
	}
	newH := int(math.RoundToEven(float64(sh) * float64(w) / float64(sw)))
	if newH < 1 {
		newH = 1
	}
	srcRect := src.Bounds()
	dstH := newH
	if newH > maxH {
		dstH = maxH
		rows := int(math.Ceil(float64(maxH) * float64(sh) / float64(newH)))
		if rows < 1 {
			rows = 1
		}
		if rows > sh {
			rows = sh
		}
		srcRect = image.Rect(0, 0, sw, rows)
	}
	if sw == w && srcRect.Dy() == dstH {
		return src.SubImage(srcRect).(*image.RGBA)
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, dstH))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, srcRect, xdraw.Src, nil)
	return dst
}

// ditherFS converts to gray (L) and applies Floyd-Steinberg error diffusion
// (7/16, 3/16, 5/16, 1/16, left to right) with a 128 threshold. true = black.
func ditherFS(img *image.RGBA) *domain.Bitmap {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	bm := &domain.Bitmap{W: w, H: h, Pix: make([]bool, w*h)}
	cur := make([]int, w+2)
	next := make([]int, w+2)
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+w*4]
		for x := 0; x < w; x++ {
			p := row[x*4 : x*4+3]
			v := lumaL24(p[0], p[1], p[2]) + cur[x+1]
			var out int
			if v >= 128 {
				out = 255
			} else {
				bm.Pix[y*w+x] = true
			}
			e := v - out
			cur[x+2] += e * 7 / 16
			next[x] += e * 3 / 16
			next[x+1] += e * 5 / 16
			next[x+2] += e * 1 / 16
		}
		cur, next = next, cur
		for i := range next {
			next[i] = 0
		}
	}
	return bm
}

const asciiRamp = "@%#*+=-:. " // dark -> light

// bitmapToASCII converts a raster to ASCII art, columns wide. Characters are
// about twice as tall as wide, so rows = round(cols * h / w * 0.5).
func bitmapToASCII(b *domain.Bitmap, columns int) string {
	if b == nil || b.W <= 0 || b.H <= 0 || len(b.Pix) < b.W*b.H {
		return ""
	}
	if columns < 1 {
		columns = 1
	}
	rows := int(math.RoundToEven(float64(columns) * float64(b.H) / float64(b.W) * 0.5))
	if rows < 1 {
		rows = 1
	}
	n := len(asciiRamp) - 1
	var sb strings.Builder
	for y := 0; y < rows; y++ {
		y0, y1 := y*b.H/rows, (y+1)*b.H/rows
		if y1 <= y0 {
			y1 = y0 + 1
		}
		line := make([]byte, columns)
		for x := 0; x < columns; x++ {
			x0, x1 := x*b.W/columns, (x+1)*b.W/columns
			if x1 <= x0 {
				x1 = x0 + 1
			}
			white, cnt := 0, 0
			for yy := y0; yy < y1 && yy < b.H; yy++ {
				base := yy * b.W
				for xx := x0; xx < x1 && xx < b.W; xx++ {
					if !b.Pix[base+xx] {
						white++
					}
					cnt++
				}
			}
			px := 255
			if cnt > 0 {
				px = white * 255 / cnt
			}
			line[x] = asciiRamp[px*n/255]
		}
		if y > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(strings.TrimRight(string(line), " "))
	}
	return sb.String()
}

// bitmapGray renders a Bitmap as a Gray image (ink = 0, paper = 255).
func bitmapGray(b *domain.Bitmap) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, b.W, b.H))
	for i, on := range b.Pix {
		if i >= len(g.Pix) {
			break
		}
		if on {
			g.Pix[i] = 0
		} else {
			g.Pix[i] = 255
		}
	}
	return g
}
