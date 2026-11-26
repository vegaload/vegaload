package netapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

// SleepArgs reads the arguments of sleep(seconds) and sleep(min, max), in
// both JavaScript and Python. Seconds may have a fraction. With two
// arguments the time is picked at random, evenly, between min and max.
func SleepArgs(args []any) (time.Duration, error) {
	switch len(args) {
	case 1:
		s, err := sleepSeconds(args[0])
		if err != nil {
			return 0, err
		}
		return seconds(s), nil
	case 2:
		lo, err := sleepSeconds(args[0])
		if err != nil {
			return 0, err
		}
		hi, err := sleepSeconds(args[1])
		if err != nil {
			return 0, err
		}
		if lo > hi {
			return 0, fmt.Errorf("sleep: min %g is more than max %g", lo, hi)
		}
		return seconds(lo + rand.Float64()*(hi-lo)), nil //nolint:gosec // think time, not a secret
	default:
		return 0, errors.New("sleep: want sleep(seconds) or sleep(min, max)")
	}
}

func sleepSeconds(v any) (float64, error) {
	n, ok := number(v)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("sleep: want a number of seconds, like 1.5, not %v", v)
	}
	if n < 0 {
		return 0, fmt.Errorf("sleep: want zero or more seconds, not %g", n)
	}
	return n, nil
}

func seconds(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}

// Sleep waits d, or less when ctx ends first, so a sleeping VU never holds
// up the end of a run. It reports whether it waited the whole time.
func Sleep(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
