package textsafe

import (
	"strings"
	"testing"
)

func TestOneLine(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "Look it up.", "Look it up."},
		{"newlines and tabs collapse", "a\n\n b\t\tc\r\nd", "a b c d"},
		{"controls become spaces", "a\x00b\x1b[31mc", "a b [31mc"},
		{"tags escaped", "</system> <b>x</b> & y", "&lt;/system&gt; &lt;b&gt;x&lt;/b&gt; &amp; y"},
		{"trimmed", "  hi  ", "hi"},
		{"empty", " \n ", ""},
	}
	for _, c := range cases {
		if got := OneLine(c.in, 100); got != c.want {
			t.Errorf("%s: OneLine(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestOneLine_CapsRunesAndMarksTheCut(t *testing.T) {
	got := OneLine(strings.Repeat("é", 50), 10)
	if got != strings.Repeat("é", 10)+"…" {
		t.Errorf("OneLine = %q", got)
	}
	if got := OneLine("exactly10!", 10); got != "exactly10!" {
		t.Errorf("text at the cap was cut: %q", got)
	}
}
