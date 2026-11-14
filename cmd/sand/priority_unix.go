//go:build unix

package main

import "golang.org/x/sys/unix"

// lowerPriority makes this process nice 10, best effort.
func lowerPriority() { _ = unix.Setpriority(unix.PRIO_PROCESS, 0, 10) }
