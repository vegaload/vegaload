//go:build !unix && !windows

package genload

import "time"

func processCPU() (time.Duration, bool) { return 0, false }
