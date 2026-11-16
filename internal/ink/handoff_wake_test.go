package ink

import (
	"testing"
	"time"
)

func TestTaskWakeRetryDelay(t *testing.T) {
	for _, tc := range []struct {
		attempts int
		minutes  int
	}{
		{1, 1}, {2, 1}, {3, 1}, {4, 2}, {5, 3}, {6, 4},
		{61, 59}, {62, 60}, {63, 60}, {1000000, 60},
	} {
		if got := taskWakeRetryDelay(tc.attempts); got != time.Duration(tc.minutes)*time.Minute {
			t.Errorf("after wake %d: got %s, want %d minutes", tc.attempts, got, tc.minutes)
		}
	}
}
