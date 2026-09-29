// Package download fetches a task's files from the server.
package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// File downloads src to dst through dst.part, checking wantSHA (hex) when
// it is set; dst appears only once the file is complete.
func File(ctx context.Context, src, dst, wantSHA string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", src, resp.Status)
	}
	tmp := dst + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if wantSHA != "" && !strings.EqualFold(sum, wantSHA) {
		os.Remove(tmp)
		return fmt.Errorf("sha256 mismatch: got %s want %s", sum, wantSHA)
	}
	return os.Rename(tmp, dst)
}
