package render

import (
	"image"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
	"github.com/badhrinadhbade/pos-ticker/internal/render/fonts"
)

// Receipt font names accepted by the receipt_font setting.
const (
	FontReceipt = "receipt"
	FontMono    = "mono"
	FontSans    = "sans"
	FontSerif   = "serif"
)

var (
	parseOnce sync.Once
	parsed    map[string]*sfnt.Font
	parseErr  error
)

func loadFonts() (map[string]*sfnt.Font, error) {
	parseOnce.Do(func() {
		parsed = map[string]*sfnt.Font{}
		for name, data := range map[string][]byte{
			FontReceipt: fonts.MerchantCopy,
			FontMono:    fonts.DejaVuSansMono,
			FontSans:    fonts.DejaVuSans,
			FontSerif:   fonts.DejaVuSerif,
			"emoji":     fonts.NotoEmoji,
		} {
			f, err := opentype.Parse(data)
			if err != nil {
				parseErr = err
				return
			}
			parsed[name] = f
		}
	})
	return parsed, parseErr
}

// normalizeFontName maps unknown names to the default "receipt" face.
func normalizeFontName(s string) string {
	switch n := strings.ToLower(strings.TrimSpace(s)); n {
	case FontMono, FontSans, FontSerif, FontReceipt:
		return n
	}
	return FontReceipt
}

// faceChain is an ordered per-rune fallback list: requested face, DejaVu Sans
// (accents/Greek/Cyrillic/symbols), then monochrome Noto Emoji.
// A chain is NOT safe for concurrent use (opentype faces cache glyphs).
type faceChain struct {
	faces    []font.Face
	names    []string
	size     float64
	emojiIdx int
}

func newFaceChain(fontName string, size float64) (*faceChain, error) {
	all, err := loadFonts()
	if err != nil {
		return nil, err
	}
	fontName = normalizeFontName(fontName)
	names := []string{fontName}
	if fontName != FontSans {
		names = append(names, FontSans)
	}
	names = append(names, "emoji")
	c := &faceChain{size: size, emojiIdx: len(names) - 1, names: names}
	for _, n := range names {
		fs := size
		switch {
		case n == "emoji":
			fs = size * 0.9
		case n == FontSans && fontName == FontReceipt:
			// Merchant Copy has a very small x-height; scale the DejaVu
			// fallback so accented letters sit next to it at a similar size.
			fs = size * 0.78
		}
		face, err := opentype.NewFace(all[n], &opentype.FaceOptions{Size: fs, DPI: 72, Hinting: font.HintingNone})
		if err != nil {
			c.Close()
			return nil, err
		}
		c.faces = append(c.faces, face)
	}
	return c, nil
}

func (c *faceChain) Close() {
	for _, f := range c.faces {
		f.Close()
	}
}

// pick returns the index of the first face that has a glyph for r, or -1.
func (c *faceChain) pick(r rune) int {
	for i, f := range c.faces {
		if _, ok := f.GlyphAdvance(r); ok {
			return i
		}
	}
	return -1
}

// zeroWidth reports runes that must never be drawn on their own: joiners,
// variation selectors, keycap/skin-tone/tag modifiers (no GSUB ligation here)
// and invisible format/combining characters.
func zeroWidth(r rune) bool {
	switch {
	case r == 0x200D, r == 0xFE0E, r == 0xFE0F, r == 0x20E3:
		return true
	case r >= 0x1F3FB && r <= 0x1F3FF: // skin tone modifiers
		return true
	case r >= 0xE0020 && r <= 0xE007F: // tag characters
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf)
}

// glyph resolves r to (face index, advance). ok=false means "draw nothing".
func (c *faceChain) glyph(r rune) (idx int, adv fixed.Int26_6, ok bool) {
	if zeroWidth(r) {
		return 0, 0, false
	}
	idx = c.pick(r)
	if idx < 0 {
		r = '?'
		idx = c.pick(r)
		if idx < 0 {
			return 0, 0, false
		}
	}
	a, _ := c.faces[idx].GlyphAdvance(r)
	return idx, a, true
}

func (c *faceChain) measure(s string) fixed.Int26_6 {
	var w fixed.Int26_6
	for _, r := range s {
		if _, a, ok := c.glyph(r); ok {
			w += a
		}
	}
	return w
}

// wrapPixels word-wraps text to maxW pixels using measured advances. Words wider
// than a line are split by rune. Blank paragraphs yield empty lines.
func (c *faceChain) wrapPixels(text string, maxW fixed.Int26_6) []string {
	space := c.measure(" ")
	var lines []string
	for _, para := range strings.Split(text, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			lines = append(lines, "")
			continue
		}
		line, lineW := "", fixed.Int26_6(0)
		flush := func() {
			if line != "" {
				lines = append(lines, line)
			}
			line, lineW = "", 0
		}
		for _, word := range words {
			ww := c.measure(word)
			switch {
			case line == "" && ww <= maxW:
				line, lineW = word, ww
			case line != "" && lineW+space+ww <= maxW:
				line, lineW = line+" "+word, lineW+space+ww
			default:
				flush()
				if ww <= maxW {
					line, lineW = word, ww
					break
				}
				// Overlong word: break by rune.
				for _, r := range word {
					_, a, ok := c.glyph(r)
					if !ok {
						line += string(r)
						continue
					}
					if lineW+a > maxW && line != "" {
						flush()
					}
					line += string(r)
					lineW += a
				}
			}
		}
		flush()
	}
	if len(lines) == 0 {
		lines = []string{""}
	}
	return lines
}

// drawLine draws s with its baseline at (x, base) onto dst (ink = black).
func (c *faceChain) drawLine(dst *image.Gray, s string, x fixed.Int26_6, base int) {
	d := &font.Drawer{Dst: dst, Src: image.Black}
	for _, r := range s {
		idx, adv, ok := c.glyph(r)
		if !ok {
			continue
		}
		g := r
		if c.pick(r) < 0 {
			g = '?'
		}
		d.Face = c.faces[idx]
		d.Dot = fixed.Point26_6{X: x, Y: fixed.I(base)}
		d.DrawString(string(g))
		x += adv
	}
}

// usesEmoji reports whether any rune of s resolves to the emoji face.
func (c *faceChain) usesEmoji(s string) bool {
	for _, r := range s {
		if zeroWidth(r) {
			continue
		}
		if c.pick(r) == c.emojiIdx {
			return true
		}
	}
	return false
}

// renderTextBitmap draws text, wrapped by measured pixel width, at widthPx wide
// in the given font, then thresholds to 1 bit (<200 = ink), like the Python
// _render_message_image. Line pitch = size + 8.
func renderTextBitmap(text, fontName string, size, widthPx int) (*domain.Bitmap, error) {
	if size < 6 {
		size = 6
	}
	chain, err := newFaceChain(fontName, float64(size))
	if err != nil {
		return nil, err
	}
	defer chain.Close()

	text = cleanForBitmap(text)
	lines := limitBodyLines(chain.wrapPixels(text, fixed.I(widthPx)))
	lineH := size + 8
	h := len(lines) * lineH
	if h < lineH {
		h = lineH
	}
	g := image.NewGray(image.Rect(0, 0, widthPx, h))
	for i := range g.Pix {
		g.Pix[i] = 255
	}
	// Baseline: the primary face's ascent; lifted so emoji (taller than
	// Merchant Copy's tiny ascent) are not clipped at the top of the line.
	asc := chain.faces[0].Metrics().Ascent.Ceil()
	if chain.usesEmoji(text) {
		if min := int(float64(size) * 0.9); asc < min {
			asc = min
		}
	}
	for i, ln := range lines {
		chain.drawLine(g, ln, 0, i*lineH+asc)
	}
	bm := &domain.Bitmap{W: widthPx, H: h, Pix: make([]bool, widthPx*h)}
	for i, v := range g.Pix {
		bm.Pix[i] = v < 200
	}
	return bm, nil
}
