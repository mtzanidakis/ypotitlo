//go:build !windows

package main

import "syscall"

// umask reads the process umask without changing it. Setting it to itself is the
// only way to read it on Unix; the call is not thread-safe, so it happens once
// on the way to writing a single file rather than concurrently.
func umask() int {
	m := syscall.Umask(0)
	syscall.Umask(m)
	return m
}
