package discordrpc

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// This file is the whole Discord IPC client. It is deliberately small: the
// integration needs only a handshake, SET_ACTIVITY and a close, so there is no
// reason to pull a general RPC library in for it. Writing it here also lets the
// activity carry the fields the wire format actually accepts, including the
// activity name Discord draws in the card header.
//
// The frame is Discord's: a 4-byte little-endian opcode, a 4-byte little-endian
// payload length, then the UTF-8 JSON payload.

// Discord IPC opcodes.
const (
	opHandshake = 0
	opFrame     = 1
	opClose     = 2
	opPing      = 3
	opPong      = 4
)

// ipcTimeout bounds one request or response, so a Discord that stopped
// answering cannot pin the manager's loop. On a timeout the connection is
// dropped and the retry tick reconnects.
const ipcTimeout = 3 * time.Second

// maxFrame caps a single IPC payload. Presence frames are tiny; the bound only
// stops a corrupt length prefix from asking for a wild allocation.
const maxFrame = 1 << 20

// ipcClient owns one connection to the local Discord client. It is not safe
// for concurrent use; the manager's loop is the only goroutine that touches it.
type ipcClient struct {
	appID string
	conn  net.Conn
}

func newIPCClient(appID string) *ipcClient {
	return &ipcClient{appID: appID}
}

// clientFrame is the payload of the opening handshake (opcode 0) and the
// closing frame (opcode 2): both carry just the RPC version and the client id.
type clientFrame struct {
	Version  int    `json:"v"`
	ClientID string `json:"client_id"`
}

// ipcError is the shape of a frame Discord sends when a command is rejected.
type ipcError struct {
	Evt  string `json:"evt"`
	Data struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"data"`
}

// login opens the first Discord socket that answers and completes the
// handshake. It fails when Discord is not running, which the manager treats as
// "try again on the next retry tick".
func (c *ipcClient) login() error {
	conn, err := dialDiscord()
	if err != nil {
		return fmt.Errorf("discord ipc: no socket: %w", err)
	}
	c.conn = conn

	if err := c.writeFrame(opHandshake, clientFrame{Version: 1, ClientID: c.appID}); err != nil {
		c.close()

		return fmt.Errorf("discord ipc: handshake: %w", err)
	}

	payload, err := c.await()
	if err != nil {
		c.close()

		return fmt.Errorf("discord ipc: handshake reply: %w", err)
	}
	if err := discordError(payload); err != nil {
		c.close()

		return fmt.Errorf("discord ipc: handshake rejected: %w", err)
	}

	return nil
}

// logout asks Discord to close the session, then drops the socket. The close
// frame is best effort: the socket teardown is what actually ends the presence.
func (c *ipcClient) logout() {
	if c.conn == nil {
		return
	}
	_ = c.writeFrame(opClose, clientFrame{Version: 1, ClientID: c.appID})
	c.close()
}

// setActivity publishes one activity. A nil activity clears the presence, which
// is how an empty queue is represented. It waits for Discord's reply so a
// rejected frame is surfaced as a dropped connection rather than silently lost.
func (c *ipcClient) setActivity(a *activity) error {
	if c.conn == nil {
		return fmt.Errorf("discord ipc: not connected")
	}

	nonce, err := newNonce()
	if err != nil {
		return err
	}

	frame := struct {
		Cmd  string `json:"cmd"`
		Args struct {
			PID      int           `json:"pid"`
			Activity *wireActivity `json:"activity"`
		} `json:"args"`
		Nonce string `json:"nonce"`
	}{
		Cmd:   "SET_ACTIVITY",
		Nonce: nonce,
	}
	frame.Args.PID = os.Getpid()
	frame.Args.Activity = a.wire()

	if err := c.writeFrame(opFrame, frame); err != nil {
		return fmt.Errorf("discord ipc: set activity: %w", err)
	}

	payload, err := c.await()
	if err != nil {
		return fmt.Errorf("discord ipc: set activity reply: %w", err)
	}

	return discordError(payload)
}

// await reads frames until a command reply (opcode 1) arrives, answering any
// keepalive ping in between. It returns the raw reply payload.
func (c *ipcClient) await() ([]byte, error) {
	for {
		op, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case opPing:
			if err := c.writeRaw(opPong, payload); err != nil {
				return nil, err
			}
		case opFrame:
			return payload, nil
		case opClose:
			return nil, fmt.Errorf("discord closed the connection")
		}
	}
}

// discordError reports a frame as an error when Discord marked it one.
func discordError(payload []byte) error {
	var e ipcError
	if err := json.Unmarshal(payload, &e); err != nil {
		return nil
	}
	if e.Evt != "ERROR" {
		return nil
	}

	return fmt.Errorf("%s (code %d)", e.Data.Message, e.Data.Code)
}

// writeFrame marshals a payload and sends it as one frame.
func (c *ipcClient) writeFrame(op int, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}

	return c.writeRaw(op, payload)
}

// writeRaw sends an already-encoded payload as one frame.
func (c *ipcClient) writeRaw(op int, payload []byte) error {
	if len(payload) > maxFrame {
		return fmt.Errorf("frame length %d out of range", len(payload))
	}

	buf := make([]byte, 8+len(payload))
	binary.LittleEndian.PutUint32(buf[0:4], uint32(op))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(payload)))
	copy(buf[8:], payload)

	if err := c.conn.SetWriteDeadline(time.Now().Add(ipcTimeout)); err != nil {
		return err
	}

	_, err := c.conn.Write(buf)

	return err
}

// readFrame reads one frame, returning its opcode and payload.
func (c *ipcClient) readFrame() (int, []byte, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(ipcTimeout)); err != nil {
		return 0, nil, err
	}

	var header [8]byte
	if _, err := io.ReadFull(c.conn, header[:]); err != nil {
		return 0, nil, err
	}

	op := int(binary.LittleEndian.Uint32(header[0:4]))
	length := int(binary.LittleEndian.Uint32(header[4:8]))
	if length < 0 || length > maxFrame {
		return 0, nil, fmt.Errorf("frame length %d out of range", length)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(c.conn, payload); err != nil {
		return 0, nil, err
	}

	return op, payload, nil
}

func (c *ipcClient) close() {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

// newNonce is a UUIDv4-shaped unique id for one command, which Discord echoes
// in the reply. It only has to be unique per connection, so random bytes are
// enough.
func newNonce() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	buf[6] = (buf[6] & 0x0f) | 0x40 // version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 10

	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:]), nil
}
