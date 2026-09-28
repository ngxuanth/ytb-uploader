//go:build windows

package chromectl

import "testing"

func TestChromeBrowserMatchDoesNotPrefixMatch(t *testing.T) {
	flag := "--user-data-dir=C:\\chrome-profile"
	if !chromeBrowserMatch(flag+" --no-first-run", flag) {
		t.Fatal("exact path should match")
	}
	if chromeBrowserMatch("--user-data-dir=C:\\chrome-profile-ports\\isophtalic", flag) {
		t.Fatal("longer path should not match")
	}
	if chromeBrowserMatch(flag+" --type=renderer", flag) {
		t.Fatal("renderer should not match")
	}
}
