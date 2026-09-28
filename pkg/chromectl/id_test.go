//go:build unix

package chromectl

import "testing"

func TestUnpackedID(t *testing.T) {
	// The id Chrome gave the extension loaded from this path.
	if got := unpackedID("/home/isophtalic/Workspace/BrowserMCP/browsermcp-extension"); got != "djibjaoaohncgoahienfcipkhdgpdgpi" {
		t.Fatalf("unpackedID = %s", got)
	}
}
