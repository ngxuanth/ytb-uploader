package contract

import (
	"testing"
	"time"
)

func TestUploadBudgetGrowsWithTheFile(t *testing.T) {
	for _, tc := range []struct {
		size int64
		want time.Duration
	}{
		{0, 20 * time.Minute},
		{2 << 20, 20*time.Minute + 4*time.Second},
		{1 << 30, 20*time.Minute + 2048*time.Second},
	} {
		if got := UploadBudget(tc.size); got != tc.want {
			t.Errorf("UploadBudget(%d) = %s, want %s", tc.size, got, tc.want)
		}
	}
}
