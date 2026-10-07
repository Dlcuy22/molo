//go:build !windows

package discordrpc

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// dialTimeout bounds one connect attempt. It is short so scanning the candidate
// sockets of a missing Discord stays cheap.
const dialTimeout = 2 * time.Second

// dialDiscord connects to the local Discord IPC socket. Discord exposes ten
// sockets, discord-ipc-0 through discord-ipc-9, under the first directory that
// holds one; it picks the lowest free index when it starts. The directories
// follow Discord's own resolution order, with the snap and flatpak sandbox
// paths checked first because those installs are common on Linux.
func dialDiscord() (net.Conn, error) {
	for _, dir := range ipcDirs() {
		for i := 0; i < 10; i++ {
			path := filepath.Join(dir, "discord-ipc-"+strconv.Itoa(i))
			conn, err := net.DialTimeout("unix", path, dialTimeout)
			if err == nil {
				return conn, nil
			}
		}
	}

	return nil, os.ErrNotExist
}

// ipcDirs is the candidate set of socket directories, most specific first.
func ipcDirs() []string {
	var dirs []string
	if runtime := os.Getenv("XDG_RUNTIME_DIR"); runtime != "" {
		dirs = append(dirs,
			filepath.Join(runtime, "snap.discord"),
			filepath.Join(runtime, ".flatpak", "com.discordapp.Discord", "xdg-run"),
		)
	}
	for _, v := range []string{"XDG_RUNTIME_DIR", "TMPDIR", "TMP", "TEMP"} {
		if dir := os.Getenv(v); dir != "" {
			dirs = append(dirs, dir)
		}
	}

	return append(dirs, "/tmp")
}
