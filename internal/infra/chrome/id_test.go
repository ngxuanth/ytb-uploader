//go:build unix

package chrome

import "testing"

func TestUnpackedID(t *testing.T) {
	// The id Chrome gave the extension loaded from this path.
	if got := unpackedID("/home/isophtalic/Workspace/BrowserMCP/browsermcp-extension"); got != "djibjaoaohncgoahienfcipkhdgpdgpi" {
		t.Fatalf("unpackedID = %s", got)
	}
}
