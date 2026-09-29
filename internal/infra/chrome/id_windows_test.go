//go:build windows

package chrome

import "testing"

func TestWindowsUnpackedIDMatchesChrome(t *testing.T) {
	// Chrome 154 hashed this path as UTF-16LE and returned this id from Extensions.loadUnpacked.
	const path = `C:\Users\Admin\Workspace\Ytbuploader\resource\browsermcp-extension`
	if got := extensionID(pathIDBytes(path)); got != "bdfiomipcldbakkognklngkhfmbcacgh" {
		t.Fatalf("extensionID = %s", got)
	}
	lower := `c:\Users\Admin\Workspace\Ytbuploader\resource\browsermcp-extension`
	if got := extensionID(pathIDBytes(lower)); got != "bdfiomipcldbakkognklngkhfmbcacgh" {
		t.Fatalf("lowercase drive = %s", got)
	}
}
