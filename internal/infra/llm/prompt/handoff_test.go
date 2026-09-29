package prompt

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateKeepsUTF8(t *testing.T) {
	s := strings.Repeat("các video khác", 100) // multi-byte Vietnamese letters
	for n := 1; n < 60; n++ {
		if got := truncate(s, n); !utf8.ValidString(got) {
			t.Fatalf("n=%d: invalid UTF-8 %q", n, got)
		}
	}
	if truncate("abc", 10) != "abc" {
		t.Fatal("short string changed")
	}
}
