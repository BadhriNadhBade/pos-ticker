// Package fonts embeds the (subset) typefaces used by the receipt renderer.
// See LICENSES.md in this directory for the license of each file.
package fonts

import _ "embed"

// MerchantCopy is the classic thermal "store receipt" face (unmodified).
//
//go:embed merchant-copy.ttf
var MerchantCopy []byte

// DejaVuSansMono, DejaVuSans and DejaVuSerif are subsets covering Basic Latin,
// Latin-1/Extended, (Sans: Greek, Cyrillic, Latin Extended Additional), general
// punctuation, currency, arrows, math and box/geometric symbols.
//
//go:embed DejaVuSansMono.ttf
var DejaVuSansMono []byte

//go:embed DejaVuSans.ttf
var DejaVuSans []byte

//go:embed DejaVuSerif.ttf
var DejaVuSerif []byte

// NotoEmoji is the monochrome Noto Emoji face (static wght=500 instance, subset
// to emoji ranges; no layout tables, so ZWJ sequences are not ligated).
//
//go:embed NotoEmoji.ttf
var NotoEmoji []byte
