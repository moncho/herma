package safetext

import "testing"

func TestVisibleText(t *testing.T) {
	for name, tc := range map[string]struct{ in, line, block string }{
		"plain":           {"Prefer résumé — 日本語", "Prefer résumé — 日本語", "Prefer résumé — 日本語"},
		"line breaks":     {"a\nb\tc\r", `a\nb\tc\r`, "a\nb\tc" + `\r`},
		"escape sequence": {"a\x1b[2Kb", `a\x1b[2Kb`, `a\x1b[2Kb`},
		"c1 and delete":   {"a\u009b\x7f", `a\x9b\x7f`, `a\x9b\x7f`},
		"bidi override":   {"a\u202eb\u2066", `a\u202eb\u2066`, `a\u202eb\u2066`},
		"line separator":  {"a\u2028b", `a\u2028b`, `a\u2028b`},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Line(tc.in); got != tc.line {
				t.Errorf("Line: %q, want %q", got, tc.line)
			}
			if got := Block(tc.in); got != tc.block {
				t.Errorf("Block: %q, want %q", got, tc.block)
			}
		})
	}
}
