package render

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
	"github.com/badhrinadhbade/pos-ticker/internal/escpos"
)

// Thermal head geometry used by the preview compositor (Font A 12x24, Font B
// 9x17 dots, 30-dot default line pitch, as on typical 203 dpi 80mm printers).
const (
	cellAW, cellAH       = 12, 24
	cellBW, cellBH       = 9, 17
	defaultLineSpacing   = 30
	previewMargin        = 12
	monoAdvanceEm        = 0.6015 // DejaVu Sans Mono advance / em
	previewCutGap        = 14
	previewDashLen       = 8
	previewDashGap       = 6
	previewDashThickness = 2
)

type pItem struct {
	y     int
	x     int
	text  string
	bold  bool
	fontB bool
	bm    *domain.Bitmap
	cutAt bool
}

// composePreview lays an op list out on simulated paper widthPx wide. It follows
// the same state machine as an ESC/POS printer (alignment, bold, Font B,
// line spacing, feed) but needs no byte parsing: the text and rasters are the
// very ops BuildJob replays onto the Builder.
func composePreview(ops opList, widthPx int) (*image.Gray, error) {
	var (
		items   []pItem
		al      = escpos.Left
		bold    bool
		fontB   bool
		spacing = defaultLineSpacing
		y       = previewMargin
		line    strings.Builder
		lineAl  = escpos.Left
	)
	cw := func() int {
		if fontB {
			return cellBW
		}
		return cellAW
	}
	place := func(a escpos.Align, w int) int {
		switch a {
		case escpos.Center:
			if x := (widthPx - w) / 2; x > 0 {
				return x
			}
		case escpos.Right:
			if x := widthPx - w; x > 0 {
				return x
			}
		}
		return 0
	}
	flushLine := func() {
		t := line.String()
		line.Reset()
		if t != "" {
			n := len([]rune(t))
			items = append(items, pItem{y: y, x: place(lineAl, n*cw()), text: t, bold: bold, fontB: fontB})
		}
		y += spacing // Font B glyphs are shorter but the line pitch is the same
	}
	for _, o := range ops {
		switch o.k {
		case opInit:
			al, bold, fontB, spacing = escpos.Left, false, false, defaultLineSpacing
		case opAlign:
			al = o.align
		case opBold:
			bold = o.on
		case opFontB:
			fontB = o.on
		case opSpacing:
			if o.n <= 0 {
				spacing = defaultLineSpacing
			} else {
				spacing = o.n
			}
		case opText:
			for _, r := range o.s {
				if r == '\n' {
					flushLine()
					continue
				}
				if line.Len() == 0 {
					lineAl = al
				}
				line.WriteRune(r)
			}
		case opFeed:
			if line.Len() > 0 {
				flushLine()
			}
			y += o.n * spacing
		case opRaster:
			if o.bm == nil {
				continue
			}
			items = append(items, pItem{y: y, x: place(al, o.bm.W), bm: o.bm})
			y += o.bm.H
		case opCut:
			if line.Len() > 0 {
				flushLine()
			}
			y += previewCutGap
			items = append(items, pItem{y: y, cutAt: true})
			y += previewCutGap
		}
	}
	if line.Len() > 0 {
		flushLine()
	}
	height := y + previewMargin
	if height < 2*previewMargin+1 {
		height = 2*previewMargin + 1
	}
	if widthPx <= 0 || height > 200000 {
		return nil, errors.New("render: invalid preview size")
	}
	canvas := image.NewGray(image.Rect(0, 0, widthPx, height))
	for i := range canvas.Pix {
		canvas.Pix[i] = 255
	}

	all, err := loadFonts()
	if err != nil {
		return nil, err
	}
	faceA, err := opentype.NewFace(all[FontMono], &opentype.FaceOptions{Size: float64(cellAW) / monoAdvanceEm, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		return nil, err
	}
	defer faceA.Close()
	faceB, err := opentype.NewFace(all[FontMono], &opentype.FaceOptions{Size: float64(cellBW) / monoAdvanceEm, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		return nil, err
	}
	defer faceB.Close()

	for _, it := range items {
		switch {
		case it.bm != nil:
			blitBitmap(canvas, it.bm, it.x, it.y)
		case it.cutAt:
			for x := 0; x < widthPx; x += previewDashLen + previewDashGap {
				for dx := 0; dx < previewDashLen && x+dx < widthPx; dx++ {
					for t := 0; t < previewDashThickness; t++ {
						canvas.SetGray(x+dx, it.y+t, color.Gray{Y: 0})
					}
				}
			}
		default:
			face, cell, base := faceA, cellAW, 19
			if it.fontB {
				face, cell, base = faceB, cellBW, 14
			}
			d := &font.Drawer{Dst: canvas, Src: image.Black, Face: face}
			for i, r := range []rune(it.text) {
				if r == ' ' {
					continue
				}
				for pass := 0; pass < 1+b2i(it.bold); pass++ {
					d.Dot = fixed.P(it.x+i*cell+pass, it.y+base)
					d.DrawString(string(r))
				}
			}
		}
	}
	return canvas, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func blitBitmap(dst *image.Gray, bm *domain.Bitmap, x0, y0 int) {
	for y := 0; y < bm.H; y++ {
		dy := y0 + y
		if dy < 0 || dy >= dst.Bounds().Dy() {
			continue
		}
		for x := 0; x < bm.W; x++ {
			dx := x0 + x
			if dx < 0 || dx >= dst.Bounds().Dx() {
				continue
			}
			if bm.Pix[y*bm.W+x] {
				dst.Pix[dy*dst.Stride+dx] = 0
			}
		}
	}
}

func encodePNG(g image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, g); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
