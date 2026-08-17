package dnscache

import (
	"testing"
	"time"
)

func TestProbeTTLRemainingUsesWholeSecondsFloor(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewWithClock(func() time.Time { return now })
	if err := c.Put("fractional.test", TypeA, 3, []string{"1.2.3.4"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	now = now.Add(1500 * time.Millisecond)
	res := c.Lookup("fractional.test", TypeA)
	if res.Status != StatusNOERROR || res.TTLRemaining != 1 {
		t.Fatalf("lookup: %+v want NOERROR with TTLRemaining=1", res)
	}
}
