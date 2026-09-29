//go:build unix

package chromectl

import "testing"

func TestIsChromeBrowserFor(t *testing.T) {
	const dir = "/home/me/up loader/profile"
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"nul separated", "/opt/google/chrome/chrome\x00--user-data-dir=" + dir + "\x00--profile-directory=p\x00", true},
		// Chrome rewrites its cmdline into one space-joined string.
		{"space joined", "/opt/google/chrome/chrome --user-data-dir=" + dir + " --profile-directory=p --remote-debugging-port=9222 about:blank", true},
		{"helper process", "/opt/google/chrome/chrome --type=renderer --user-data-dir=" + dir, false},
		{"other dir", "/opt/google/chrome/chrome --user-data-dir=" + dir + "2 --profile-directory=p", false},
		{"not chrome", "/usr/bin/vim --user-data-dir=" + dir, false},
	}
	for _, tc := range cases {
		if got := isChromeBrowserFor([]byte(tc.raw), dir); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}
