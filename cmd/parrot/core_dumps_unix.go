//go:build darwin || linux

package main

import "golang.org/x/sys/unix"

func enableCoreDumps() {
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_CORE, &limit); err != nil {
		return
	}

	limit.Cur = limit.Max
	_ = unix.Setrlimit(unix.RLIMIT_CORE, &limit)
}
