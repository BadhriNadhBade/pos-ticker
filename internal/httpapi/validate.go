package httpapi

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file is a faithful port of the contact validation helpers of
// printer_server.py (_has_letters, _is_keyboard_walk, _looks_gibberish and the
// field normalisation in contact()). Quirks are preserved on purpose, e.g.
// only a-z are considered by the gibberish heuristic, so "Müller" ("mller",
// five letters, no vowel) is rejected exactly like the Python server does.

var kbRows = [...]string{"qwertyuiop", "asdfghjkl", "zxcvbnm"}

// truncRunes returns s cut to at most n runes (Python s[:n]).
func truncRunes(s string, n int) string {
	if n < 0 {
		return ""
	}
	if len(s) <= n { // fewer bytes than the cap => fewer runes too
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// cleanField mirrors `(v or "").strip()[:n]` after making the input valid UTF-8.
func cleanField(v string, n int) string {
	return truncRunes(strings.TrimSpace(strings.ToValidUTF8(v, "�")), n)
}

// cleanBrowserTime mirrors re.sub(r'[\x00-\x1f\x7f-\x9f]', ”, v.strip())[:64].
func cleanBrowserTime(v string) string {
	v = strings.TrimSpace(strings.ToValidUTF8(v, "�"))
	v = strings.Map(func(r rune) rune {
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, v)
	return truncRunes(v, 64)
}

// validEmail mirrors re.match(r'^[^@\s]+@[^@\s]+\.[^@\s]+$', s) with Unicode \s.
func validEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	if at <= 0 {
		return false
	}
	local, domain := s[:at], s[at+1:]
	if strings.ContainsRune(domain, '@') {
		return false
	}
	for _, part := range [2]string{local, domain} {
		for _, r := range part {
			if unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) {
				return false
			}
		}
	}
	// domain must contain a '.' with at least one char before and after it.
	rs := []rune(domain)
	for i := 1; i < len(rs)-1; i++ {
		if rs[i] == '.' {
			return true
		}
	}
	return false
}

func hasLetters(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

func isKeyboardWalk(word string) bool {
	for _, row := range kbRows {
		if strings.Contains(row, word) || strings.Contains(reverse(row), word) {
			return true
		}
	}
	return false
}

func reverse(s string) string {
	b := []byte(s) // ASCII rows only
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

// hasRepeatRun reports 5+ of the same character in a row (regex (.)\1{4,};
// '.' does not match '\n').
func hasRepeatRun(s string, min int) bool {
	var prev rune = -1
	run := 0
	for _, r := range s {
		if r == '\n' {
			prev, run = -1, 0
			continue
		}
		if r == prev {
			run++
		} else {
			prev, run = r, 1
		}
		if run >= min {
			return true
		}
	}
	return false
}

// isRepeatedChunk reports fullmatch of (.{2,4})\1+ on an a-z only word.
func isRepeatedChunk(word string) bool {
	for k := 2; k <= 4; k++ {
		if len(word)%k == 0 && len(word)/k >= 2 &&
			strings.Repeat(word[:k], len(word)/k) == word {
			return true
		}
	}
	return false
}

func lettersOnlyAZ(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// looksGibberish ports _looks_gibberish.
func looksGibberish(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	if hasRepeatRun(t, 5) {
		return true
	}
	for _, raw := range strings.Fields(t) {
		word := lettersOnlyAZ(raw)
		if len(word) < 4 {
			continue
		}
		if isKeyboardWalk(word) {
			return true
		}
		if len(word) >= 6 && isRepeatedChunk(word) {
			return true
		}
		if len(word) >= 5 && !strings.ContainsAny(word, "aeiouy") {
			return true
		}
	}
	return false
}

// contactInput is the normalised, still unvalidated contact payload.
type contactInput struct {
	Name, Email, Message, BrowserTime, ImageB64 string
}

// validateContact returns a user-facing error string ("" = ok). Order and
// messages match the Python server.
func validateContact(in contactInput, maxMsg int) string {
	if in.Name == "" || in.Message == "" {
		return "missing fields"
	}
	if in.Email != "" && !validEmail(in.Email) {
		return "invalid email"
	}
	if utf8.RuneCountInString(in.Message) > maxMsg {
		return "message too long (max " + strconv.Itoa(maxMsg) + " characters)"
	}
	if !hasLetters(in.Name) || looksGibberish(in.Name) {
		return "invalid name"
	}
	return ""
}
