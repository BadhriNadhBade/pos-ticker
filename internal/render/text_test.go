package render

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mapSettings is a minimal domain.Settings for tests.
type mapSettings map[string]string

func (m mapSettings) Get(k string) string { return m[k] }
func (m mapSettings) Int(k string, def int) int {
	if v, err := strconv.Atoi(m[k]); err == nil {
		return v
	}
	return def
}
func (m mapSettings) All() map[string]string { return m }
func (m mapSettings) Patch(_ context.Context, kv map[string]string) (map[string]string, error) {
	for k, v := range kv {
		m[k] = v
	}
	return m, nil
}

func TestVisualLen(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"日本語", 6}, // W
		{"ＡＢ", 4},  // fullwidth F
		{"aあb", 4}, // mixed
		{"é", 1},   // narrow accented
		{"ｱ", 1},   // halfwidth katakana is H -> 1
		{"한글", 4},  // Hangul syllables W
		{" ", 1},   // NBSP is N
		{"😀", 2},   // emoji presentation is W
		{"a\tb", 3},
	}
	for _, c := range cases {
		if got := visualLen(c.in); got != c.want {
			t.Errorf("visualLen(%q)=%d want %d", c.in, got, c.want)
		}
	}
}

func TestTruncateToWidth(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 3, "hel"},
		{"hello", 0, ""},
		{"日本語", 4, "日本"},
		{"日本語", 5, "日本"}, // would split the 3rd wide rune
		{"a日", 2, "a"},
		{"", 5, ""},
		{"héllo", 2, "hé"},
	}
	for _, c := range cases {
		if got := truncateToWidth(c.in, c.max); got != c.want {
			t.Errorf("truncateToWidth(%q,%d)=%q want %q", c.in, c.max, got, c.want)
		}
	}
}

func TestWrapText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		w    int
		want []string
	}{
		{"empty", "", 10, []string{""}},
		{"short", "hello world", 20, []string{"hello world"}},
		{"wrap", "the quick brown fox", 10, []string{"the quick", "brown fox"}},
		{"exact", "abcde fghij", 11, []string{"abcde fghij"}},
		{"long word kept whole", "supercalifragilistic hi", 8, []string{"supercalifragilistic", "hi"}},
		{"paragraphs", "a\n\nb", 10, []string{"a", "", "b"}},
		{"whitespace collapse", "a   b\tc", 10, []string{"a b c"}},
		{"blank lines only", "\n\n", 10, []string{"", "", ""}},
		{"cjk width 2", "日本 語語", 5, []string{"日本", "語語"}},
		{"trailing blank", "a\n", 10, []string{"a", ""}},
	}
	for _, c := range cases {
		got := wrapText(c.in, c.w)
		if strings.Join(got, "|") != strings.Join(c.want, "|") || len(got) != len(c.want) {
			t.Errorf("%s: wrapText(%q,%d)=%q want %q", c.name, c.in, c.w, got, c.want)
		}
	}
}

func TestKVRow(t *testing.T) {
	got := kvRow("from", "Bob", 32)
	want := "FROM  " + strings.Repeat(" ", 32-6-3) + "Bob\n"
	if got != want {
		t.Errorf("kvRow=%q want %q", got, want)
	}
	if len(strings.TrimSuffix(got, "\n")) != 32 {
		t.Errorf("row not exactly line width: %d", len(got)-1)
	}
	// Label is cut at 6 columns and uppercased.
	if g := kvRow("emailaddress", "x", 32); !strings.HasPrefix(g, "EMAILA") {
		t.Errorf("label not cut: %q", g)
	}
	// Value truncated to lw-6-2 = 24 columns.
	long := strings.Repeat("x", 40)
	g := strings.TrimSuffix(kvRow("FROM", long, 32), "\n")
	if g != "FROM  "+"  "+strings.Repeat("x", 24) {
		t.Errorf("long value row=%q", g)
	}
	// CJK truncation honours width 2 (24 cols = 12 runes).
	g = strings.TrimSuffix(kvRow("FROM", strings.Repeat("日", 20), 32), "\n")
	if !strings.HasSuffix(g, strings.Repeat("日", 12)) || strings.Count(g, "日") != 12 {
		t.Errorf("cjk value row=%q", g)
	}
}

func TestUTCToDisplay(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	// Summer (PDT, UTC-7): 2025-05-12 21:41 UTC = 2:41 PM
	if got, want := utcToDisplay("2025-05-12 21:41:05", la), "Mon  May 12  2:41 PM"; got != want {
		t.Errorf("summer: %q want %q", got, want)
	}
	// Winter (PST, UTC-8): 2025-01-05 08:09 UTC = Sun Jan 5 12:09 AM
	if got, want := utcToDisplay("2025-01-05 08:09:00", la), "Sun  Jan 5  12:09 AM"; got != want {
		t.Errorf("winter: %q want %q", got, want)
	}
	if got := utcToDisplay("garbage", la); got != "garbage" {
		t.Errorf("unparseable should pass through, got %q", got)
	}
	if got := utcToDisplay("2025-05-12 21:41:05", nil); !strings.HasSuffix(got, "9:41 PM") {
		t.Errorf("nil tz should be UTC, got %q", got)
	}
}

func TestTimestampLine(t *testing.T) {
	if got, want := timestampLine("Mon  May 12  2:41 PM"), "Mon  May 12  .  2:41 PM"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if got := timestampLine("just text"); got != "just text" {
		t.Errorf("no double space: %q", got)
	}
	if got := timestampLine("5/12/2025, 2:41:05 PM"); got != "5/12/2025, 2:41:05 PM" {
		t.Errorf("browser time: %q", got)
	}
}

func TestASCIISafe(t *testing.T) {
	cases := []struct {
		in   string
		keep bool
		want string
	}{
		{"plain", true, "plain"},
		{"café", false, "cafe"},
		{"a\x1b@b", false, "a @b"}, // ESC never reaches the printer
		{"a\x1dVb", false, "a Vb"}, // GS neither
		{"a\r\nb", true, "a\nb"},
		{"a\nb", false, "a b"},
		{"日本", false, "??"},
		{"x​y", false, "xy"}, // zero-width space (Cf) dropped
		{"tab\there", true, "tab here"},
	}
	for _, c := range cases {
		got := asciiSafe(c.in, c.keep)
		if got != c.want {
			t.Errorf("asciiSafe(%q,%v)=%q want %q", c.in, c.keep, got, c.want)
		}
		for _, r := range got {
			if (r < 0x20 && r != '\n') || r >= 0x7f {
				t.Errorf("asciiSafe(%q) leaked %U", c.in, r)
			}
		}
	}
}

func TestHasNonASCII(t *testing.T) {
	if hasNonASCII("hello\nworld !~") {
		t.Error("ascii flagged")
	}
	for _, s := range []string{"é", "😀", "a b", "日本"} {
		if !hasNonASCII(s) {
			t.Errorf("%q not flagged", s)
		}
	}
}
