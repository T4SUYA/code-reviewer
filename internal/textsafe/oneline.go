// Package textsafe reduces text that comes from outside the reviewer to forms
// that are safe to place in a prompt or a tool result.
package textsafe

import (
	"strings"
	"unicode"
)

// OneLine turns text from outside the reviewer into a single plain
// line: control characters and line breaks become spaces, runs of whitespace
// collapse, the result is cut to max runes (marked with an ellipsis) and the
// angle brackets and ampersands are escaped so it cannot form a tag.
func OneLine(s string, max int) string {
	var sb strings.Builder
	space := false
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			space = true
			continue
		}
		if space && sb.Len() > 0 {
			sb.WriteByte(' ')
		}
		space = false
		sb.WriteRune(r)
	}
	runes := []rune(sb.String())
	cut := len(runes) > max
	if cut {
		runes = runes[:max]
	}
	out := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(string(runes))
	if cut {
		out += "…"
	}
	return out
}
