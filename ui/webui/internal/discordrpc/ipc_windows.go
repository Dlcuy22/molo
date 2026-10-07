//go:build windows

package discordrpc

import (
	"net"
	"strconv"
	"time"

	npipe "gopkg.in/natefinch/npipe.v2"
)

// dialTimeout bounds one connect attempt. It is short so scanning the candidate
// pipes of a missing Discord stays cheap.
const dialTimeout = 2 * time.Second

// dialDiscord connects to the local Discord IPC named pipe. Discord exposes ten
// pipes, discord-ipc-0 through discord-ipc-9, and picks the lowest free index
// when it starts, so the first that answers is the live one.
func dialDiscord() (net.Conn, error) {
	var lastErr error
	for i := 0; i < 10; i++ {
		name := `\\.\pipe\discord-ipc-` + strconv.Itoa(i)
		conn, err := npipe.DialTimeout(name, dialTimeout)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}

	return nil, lastErr
}
