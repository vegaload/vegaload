//go:build windows

package genload

import (
	"syscall"
	"time"
)

func processCPU() (time.Duration, bool) {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, false
	}
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, false
	}
	return filetimeDuration(kernel) + filetimeDuration(user), true
}

// filetimeDuration reads a FILETIME that holds an amount of time, in
// 100-nanosecond units. Filetime.Nanoseconds would treat it as a date.
func filetimeDuration(ft syscall.Filetime) time.Duration {
	return time.Duration(int64(ft.HighDateTime)<<32|int64(ft.LowDateTime)) * 100
}
