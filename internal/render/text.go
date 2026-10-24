package render

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
	"golang.org/x/text/width"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
)

// timestampLayout is the Go spelling of the Python "%a  %b %-d  %-I:%M %p".
const timestampLayout = "Mon  Jan 2  3:04 PM"

// runeWidth returns 2 for East Asian Wide/Fullwidth runes and 1 otherwise,
// matching unicodedata.east_asian_width(ch) in ("W", "F").
func runeWidth(r rune) int {
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	return 1
}

// visualLen is the number of printer columns text occupies (CJK = 2).
func visualLen(text string) int {
	n := 0
	for _, r := range text {
		n += runeWidth(r)
	}
	return n
}

// truncateToWidth cuts text so its visual width does not exceed maxWidth.
func truncateToWidth(text string, maxWidth int) string {
	w := 0
	for i, r := range text {
		cw := runeWidth(r)
		if w+cw > maxWidth {
			return text[:i]
		}
		w += cw
	}
	return text
}

// wrapText greedily wraps text to width visual columns. Paragraphs are split on
// "\n"; blank paragraphs become empty lines; words are never broken (a word
// longer than width gets its own line), exactly like the Python original.
func wrapText(text string, width int) []string {
	var lines []string
	for _, paragraph := range strings.Split(text, "\n") {
		if strings.TrimSpace(paragraph) == "" {
			lines = append(lines, "")
			continue
		}
		line := ""
		for _, word := range strings.Fields(paragraph) {
			candidate := word
			if line != "" {
				candidate = line + " " + word
			}
			if visualLen(candidate) > width {
				if line != "" {
					lines = append(lines, line)
				}
				line = word
			} else {
				line = candidate
			}
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

// maxBodyLines caps the number of wrapped body lines printed per receipt. A
// message that needs more is cut and a final "..." line is appended, so a single
// submission cannot burn an unbounded length of paper.
const maxBodyLines = 40

// truncationMarker is the final line printed when the body was cut.
const truncationMarker = "..."

// limitBodyLines is the paper-waste guard applied to wrapped message lines on
// every path (ESC/POS text, bitmap, preview): runs of 2+ blank lines collapse to
// a single blank line, and the result is cut to maxBodyLines lines followed by
// a "..." line when anything was dropped.
func limitBodyLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	prevBlank := false
	for _, ln := range lines {
		blank := strings.TrimSpace(ln) == ""
		if blank && prevBlank {
			continue
		}
		prevBlank = blank
		if blank {
			ln = ""
		}
		out = append(out, ln)
	}
	if len(out) > maxBodyLines {
		out = append(out[:maxBodyLines], truncationMarker)
	}
	return out
}

// kvRow renders a "LABEL      value" row: fixed 6-column upper-case label, value
// right-aligned to the line width and truncated by visual width. Includes the
// trailing newline.
func kvRow(label, value string, lw int) string {
	const col = 6
	l := []rune(strings.ToUpper(label))
	if len(l) > col {
		l = l[:col]
	}
	labelOut := string(l) + strings.Repeat(" ", col-len(l))
	valueOut := truncateToWidth(value, lw-col-2)
	gap := lw - col - utf8.RuneCountInString(valueOut)
	if gap < 1 {
		gap = 1
	}
	return labelOut + strings.Repeat(" ", gap) + valueOut + "\n"
}

// utcToDisplay converts a legacy "YYYY-MM-DD HH:MM:SS" UTC string to the display
// zone formatted like "Mon  May 12  2:41 PM". Unparseable input is returned as is.
func utcToDisplay(utc string, tz *time.Location) string {
	t, err := time.ParseInLocation(domain.TimeLayout, utc, time.UTC)
	if err != nil {
		return utc
	}
	if tz == nil {
		tz = time.UTC
	}
	return t.In(tz).Format(timestampLayout)
}

// splitTimestamp splits "Mon  May 12  2:41 PM" on its last double space.
func splitTimestamp(ts string) (date, tm string) {
	i := strings.LastIndex(ts, "  ")
	if i < 0 {
		return strings.TrimSpace(ts), ""
	}
	return strings.TrimSpace(ts[:i]), strings.TrimSpace(ts[i+2:])
}

// timestampLine is the centred subtitle: "date  .  time".
func timestampLine(ts string) string {
	d, t := splitTimestamp(ts)
	if t != "" {
		return d + "  .  " + t
	}
	return d
}

// hasNonASCII reports whether s contains any rune the ESC/POS text path cannot
// carry (anything >= 0x80).
func hasNonASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return true
		}
	}
	return false
}

// asciiSafe makes s safe for escpos.Builder.Text: accents are folded
// (NFKD, marks dropped), every control character (ESC, GS, ...) and every other
// non-printable-ASCII rune is replaced by ' ' / '?'. If keepNewlines is true,
// "\n" survives ("\r" is dropped, "\t" becomes a space).
func asciiSafe(s string, keepNewlines bool) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range norm.NFKD.String(s) {
		switch {
		case r == '\n' && keepNewlines:
			b.WriteByte('\n')
		case r == '\r':
		case r >= 0x20 && r < 0x7f:
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || unicode.IsSpace(r):
			b.WriteByte(' ')
		case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Cf, r):
			// combining accent / invisible format char: drop
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}

// cleanForBitmap prepares a message body for the bitmap path: NFC, drop CR,
// control characters become spaces (newlines kept), invalid UTF-8 repaired.
func cleanForBitmap(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = norm.NFC.String(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteByte('\n')
		case r == '\r':
		case r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
