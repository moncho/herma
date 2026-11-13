// Package safetext makes stored record text safe to show on a terminal.
package safetext

import (
	"fmt"
	"strings"
	"unicode"
)

// Record text is written by agents and read by the reviewer on a terminal.
// A control character there can move the cursor, erase a line or reorder
// what is shown, so every text writer prints it as a visible escape instead:
// the text looks wrong rather than different.

// Line makes s safe to print on one line: every control character, line
// breaks included, becomes a visible escape.
func Line(s string) string { return visibleText(s, false) }

// Block is Line for text that may span lines: "\n" and tab stay.
func Block(s string) string { return visibleText(s, true) }

func visibleText(s string, keepBreaks bool) string {
	if strings.IndexFunc(s, func(r rune) bool { return unsafeRune(r) }) < 0 {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case !unsafeRune(r), keepBreaks && (r == '\n' || r == '\t'):
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	return b.String()
}

// unsafeRune reports the characters a terminal acts on rather than shows:
// controls, the Unicode line and paragraph separators, and the bidirectional
// overrides and isolates that reorder the text around them.
func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || r == ' ' || r == ' ' ||
		(r >= '‪' && r <= '‮') || (r >= '⁦' && r <= '⁩')
}

// Lines applies Line to each value.
func Lines(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = Line(v)
	}
	return out
}
