package httpapi

import (
	"strings"
	"testing"
)

func TestLooksGibberish(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"   ", false},
		{"John Smith", false},
		{"Ann", false},
		{"Li", false},
		{"Jo-Ann O'Neil", false},
		{"rhythm", false}, // 'y' counts as a vowel
		{"aaaaa", true},   // 5 in a row
		{"aaaa", false},   // only 4
		{"!!!!!", true},
		{"asdf", true},   // keyboard walk
		{"qwerty", true}, // keyboard walk
		{"lkjh", true},   // reversed walk
		{"zxcv", true},
		{"qweqwe", true},     // repeated chunk
		{"asdfasdf", true},   // repeated chunk
		{"abcabc", true},     // (.{3})\1
		{"Bob xyzzy", false}, // has 'y'
		{"bcdfg", true},      // 5+ letters, no vowel
		{"bcdf", false},      // only 4 letters, not a walk
		{"Schröd", true},     // quirk kept from Python: only a-z counted => "schrd" (no vowel)
		{"Müller", false},
		{"太郎", false}, // non-latin tokens skipped
		{"x\n\n\n\n\nx", false},
	}
	for _, c := range cases {
		if got := looksGibberish(c.in); got != c.want {
			t.Errorf("looksGibberish(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestHasLetters(t *testing.T) {
	for in, want := range map[string]bool{"abc": true, "太郎": true, "é": true, "123": false, "🙂": false, "": false, "1a": true} {
		if got := hasLetters(in); got != want {
			t.Errorf("hasLetters(%q) = %v", in, got)
		}
	}
}

func TestValidEmail(t *testing.T) {
	cases := map[string]bool{
		"a@b.co": true, "a.b@c.d.e": true, "a@b.": false, "a@.b": false, "a@b": false,
		"@b.co": false, "a@@b.co": false, "a b@c.de": false, "a@b .co": false, "a@b.c@d": false,
		"x@y.z": true, "": false,
	}
	for in, want := range cases {
		if got := validEmail(in); got != want {
			t.Errorf("validEmail(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCleanFields(t *testing.T) {
	if got := cleanField("  "+strings.Repeat("é", 70)+"  ", 64); len([]rune(got)) != 64 {
		t.Errorf("rune truncation: %d", len([]rune(got)))
	}
	if got := cleanBrowserTime(" Mon\x00 May\x1b 12\x7f\u0085  2:41 PM "); got != "Mon May 12  2:41 PM" {
		t.Errorf("browser_time = %q", got)
	}
	if got := cleanBrowserTime(strings.Repeat("a", 100)); len(got) != 64 {
		t.Errorf("browser_time cap: %d", len(got))
	}
	if got := cleanField("bad\xffbytes", 64); !strings.Contains(got, "�") {
		t.Errorf("invalid utf-8 not sanitised: %q", got)
	}
}

func TestValidateContactTable(t *testing.T) {
	long := strings.Repeat("a", 501)
	ok500 := strings.Repeat("é", 500) // 500 runes, 1000 bytes: must pass the rune cap
	cases := []struct {
		name string
		in   contactInput
		want string
	}{
		{"ok", contactInput{Name: "Ada Lovelace", Email: "ada@example.com", Message: "hello"}, ""},
		{"no email ok", contactInput{Name: "Ada", Message: "hello"}, ""},
		{"ascii art / symbols ok", contactInput{Name: "Ada", Message: "!!!!! ¯\\_(ツ)_/¯ ##### 🙂"}, ""},
		{"missing name", contactInput{Message: "hi"}, "missing fields"},
		{"missing message", contactInput{Name: "Ada"}, "missing fields"},
		{"bad email", contactInput{Name: "Ada", Email: "nope", Message: "hi"}, "invalid email"},
		{"too long", contactInput{Name: "Ada", Message: long}, "message too long (max 500 characters)"},
		{"exactly 500 runes", contactInput{Name: "Ada", Message: ok500}, ""},
		{"name digits only", contactInput{Name: "12345", Message: "hi"}, "invalid name"},
		{"name emoji only", contactInput{Name: "🙂🙂", Message: "hi"}, "invalid name"},
		{"name gibberish", contactInput{Name: "asdfgh", Message: "hi"}, "invalid name"},
		{"name repeats", contactInput{Name: "Zzzzzz", Message: "hi"}, "invalid name"},
		{"name cjk", contactInput{Name: "山田太郎", Message: "hi"}, ""},
		{"email checked before length", contactInput{Name: "Ada", Email: "x", Message: long}, "invalid email"},
		{"length checked before name", contactInput{Name: "12345", Message: long}, "message too long (max 500 characters)"},
		{"message gibberish is allowed", contactInput{Name: "Ada", Message: "asdf asdf aaaaaaaa"}, ""},
	}
	for _, c := range cases {
		if got := validateContact(c.in, 500); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
