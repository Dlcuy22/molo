//go:build !windows

package main

import "os"

// attachConsole reports whether the standard streams reach a terminal. On Unix
// they already point at the launching terminal, so there is nothing to attach;
// a character device (a tty, or /dev/null) is the best cheap test available
// without a termios call, and a false result only leaves the debug pump off.
func attachConsole() bool {
	info, err := os.Stderr.Stat()
	if err != nil {
		return false
	}

	return info.Mode()&os.ModeCharDevice != 0
}
