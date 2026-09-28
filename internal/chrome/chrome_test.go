package chrome

import "testing"

func TestHasFlag(t *testing.T) {
	cases := []struct {
		cmd, flag string
		want      bool
	}{
		{"chrome --profile-directory=kenh1 --x", "--profile-directory=kenh1", true},
		{"chrome --profile-directory=kenh10 --x", "--profile-directory=kenh1", false},
		{"chrome --profile-directory=kenh10 --profile-directory=kenh1", "--profile-directory=kenh1", true},
		{`chrome.exe "--profile-directory=Profile 1" --x`, "--profile-directory=Profile 1", true},
		{"chrome --remote-debugging-port=92220", "--remote-debugging-port=9222", false},
	}
	for _, c := range cases {
		if got := hasFlag(c.cmd, c.flag); got != c.want {
			t.Errorf("hasFlag(%q, %q) = %v, muon %v", c.cmd, c.flag, got, c.want)
		}
	}
}
