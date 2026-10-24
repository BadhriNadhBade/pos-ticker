package render

import (
	"fmt"
	"strings"
	"testing"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
)

func TestLimitBodyLinesCollapsesBlankRuns(t *testing.T) {
	in := []string{"a", "", "", "", "b", " ", "", "c", ""}
	got := limitBodyLines(in)
	want := []string{"a", "", "b", "", "c", ""}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestLimitBodyLinesCap(t *testing.T) {
	var in []string
	for i := 0; i < 100; i++ {
		in = append(in, fmt.Sprintf("line %d", i))
	}
	got := limitBodyLines(in)
	if len(got) != maxBodyLines+1 || got[maxBodyLines] != "..." || got[maxBodyLines-1] != "line 39" {
		t.Fatalf("len=%d tail=%q", len(got), got[len(got)-2:])
	}
	// Exactly at the cap: untouched, no marker.
	if got := limitBodyLines(in[:maxBodyLines]); len(got) != maxBodyLines || got[len(got)-1] == "..." {
		t.Fatalf("at-cap input altered: %d", len(got))
	}
}

func TestMessageOpsTextPathLimited(t *testing.T) {
	body := "top" + strings.Repeat("\n", 30) + "bottom"
	ops, err := messageOps(sampleMsg(body), 1, defaultSettings(), Layout{}.withDefaults(), la(t))
	if err != nil {
		t.Fatal(err)
	}
	txt := textOf(ops)
	if strings.Contains(txt, "\n\n\n\n") || !strings.Contains(txt, "top\n\nbottom\n") {
		t.Errorf("blank runs not collapsed: %q", txt)
	}

	var sb strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&sb, "row%d\n", i)
	}
	ops, err = messageOps(sampleMsg(sb.String()), 1, defaultSettings(), Layout{}.withDefaults(), la(t))
	if err != nil {
		t.Fatal(err)
	}
	txt = textOf(ops)
	if !strings.Contains(txt, "row39\n...\n") || strings.Contains(txt, "row40\n") {
		t.Errorf("body not capped at %d lines: %q", maxBodyLines, txt)
	}
}

func TestBitmapPathLimited(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&sb, "é%d\n\n\n", i)
	}
	bm, err := renderTextBitmap(sb.String(), "receipt", 22, 576)
	if err != nil {
		t.Fatal(err)
	}
	if want := (maxBodyLines + 1) * (22 + 8); bm.H != want {
		t.Errorf("bitmap H=%d want %d", bm.H, want)
	}
	// Blank-line runs collapse in the bitmap path too: 3 text lines + 2 single gaps.
	bm, err = renderTextBitmap("é\n\n\n\n\nb\n\n\n\nc", "receipt", 22, 576)
	if err != nil {
		t.Fatal(err)
	}
	if want := 5 * (22 + 8); bm.H != want {
		t.Errorf("collapsed bitmap H=%d want %d", bm.H, want)
	}
}

func TestPreviewLimited(t *testing.T) {
	r := New(Config{})
	big, err := r.PreviewReceipt(domain.Message{Name: "A", Body: strings.Repeat("x\n\n\n\n", 2000)}, defaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	// Reference: a body already at the cap (40 text lines separated by single blanks).
	var capped []string
	for i := 0; i < maxBodyLines/2; i++ {
		capped = append(capped, "x", "")
	}
	ref, err := r.PreviewReceipt(domain.Message{Name: "A", Body: strings.Join(capped, "\n")}, defaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	if len(big) > 2*len(ref) {
		t.Errorf("preview of huge body (%d bytes) much larger than capped (%d)", len(big), len(ref))
	}
}
