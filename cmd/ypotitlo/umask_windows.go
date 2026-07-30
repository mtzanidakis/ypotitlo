//go:build windows

package main

// umask has no meaning on Windows, where file modes are not enforced this way.
func umask() int { return 0 }
