// Package rcon implements Minecraft's RCON command transport.
//
// RCON is a control transport only. A successful connection does not prove
// that Minecraft finished starting, so this adapter never reports readiness.
package rcon

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/liquidcatmofu/mcwarden/internal/minecraft"
)

// DefaultAddress is used when the configuration omits minecraft.rcon.address.
const DefaultAddress = "127.0.0.1:25575"

// maxPacketSize bounds network-controlled packet lengths before allocation.
// RCON packets are small; this is generous for command responses while keeping
// a malformed length from requesting a huge buffer.
const maxPacketSize = 4 << 20

const (
	typeAuthResponse int32 = 2
	typeCommand      int32 = 2
	typeAuth         int32 = 3

	authRequestID    int32 = 1
	commandRequestID int32 = 2
)

var (
	// ErrAuthentication identifies a rejected RCON password.
	ErrAuthentication = errors.New("rcon authentication failed")
	// ErrProtocol identifies a malformed or unexpected RCON packet.
	ErrProtocol = errors.New("rcon protocol error")
	// ErrPacketTooLarge identifies a packet length above the safety limit.
	ErrPacketTooLarge = errors.New("rcon packet too large")
	// ErrEmptyPassword identifies an empty password file.
	ErrEmptyPassword = errors.New("rcon password is empty")
)

// Client is a minecraft.Control that sends commands over RCON. Each Command
// opens a fresh connection, reads the password file, authenticates, writes the
// command packet, and closes. Connections are intentionally not reused across
// CLI invocations.
type Client struct {
	address      string
	passwordFile string
}

var _ minecraft.Control = (*Client)(nil)

// New builds an RCON client. An empty address uses DefaultAddress. The password
// file is read for each Command so missing or unreadable credentials do not
// prevent status, state inspection, or process-only operations.
func New(address, passwordFile string) *Client {
	if address == "" {
		address = DefaultAddress
	}
	return &Client{address: address, passwordFile: passwordFile}
}

// String and GoString keep the password out of accidental log formatting.
func (c *Client) String() string {
	if c == nil {
		return "rcon.Client<nil>"
	}
	return "rcon.Client{" + c.address + "}"
}

func (c *Client) GoString() string { return c.String() }

// Capabilities reports command transport only. Readiness stays false because an
// RCON connection is not evidence that this process generation finished
// starting.
func (c *Client) Capabilities(context.Context) (minecraft.Capabilities, error) {
	return minecraft.Capabilities{Command: true, Readiness: false}, nil
}

// Command sends command and returns once the packet has been fully written
// after successful authentication. It does not wait for the command's effect;
// in particular, "stop" may close the server before any response is read.
func (c *Client) Command(ctx context.Context, command string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	password, err := LoadPassword(c.passwordFile)
	if err != nil {
		return fmt.Errorf("rcon credentials: %w", err)
	}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", c.address)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("rcon connect: %w", err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return fmt.Errorf("rcon set deadline: %w", err)
		}
	}
	// Closing the connection unblocks a read or write when the caller cancels
	// without a deadline. stop runs before the deferred close so the callback
	// cannot race with a later connection reuse (there is none).
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if err := authenticate(conn, password); err != nil {
		return contextError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writePacket(conn, commandRequestID, typeCommand, []byte(command)); err != nil {
		return contextError(ctx, fmt.Errorf("rcon write command: %w", err))
	}
	return nil
}

func authenticate(conn net.Conn, password string) error {
	if err := writePacket(conn, authRequestID, typeAuth, []byte(password)); err != nil {
		return fmt.Errorf("rcon write auth: %w", err)
	}
	// A few server implementations send an empty response before the auth
	// response. Ignore unrelated packets briefly rather than failing on them.
	for i := 0; i < 4; i++ {
		id, packetType, _, err := readPacket(conn)
		if err != nil {
			return fmt.Errorf("rcon read auth response: %w", err)
		}
		if id == -1 {
			return ErrAuthentication
		}
		if id == authRequestID && packetType == typeAuthResponse {
			return nil
		}
	}
	return fmt.Errorf("%w: no authentication response", ErrProtocol)
}

func contextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	// The socket deadline mirrors the context deadline. Return the context
	// error even if the I/O timeout becomes visible a moment earlier.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		if _, ok := ctx.Deadline(); ok {
			return context.DeadlineExceeded
		}
	}
	return err
}

func writePacket(w io.Writer, id, packetType int32, payload []byte) error {
	length := 4 + 4 + len(payload) + 2
	if length > maxPacketSize {
		return fmt.Errorf("%w: packet length %d exceeds %d", ErrPacketTooLarge, length, maxPacketSize)
	}
	packet := make([]byte, 4+length)
	binary.LittleEndian.PutUint32(packet[0:4], uint32(length))
	binary.LittleEndian.PutUint32(packet[4:8], uint32(id))
	binary.LittleEndian.PutUint32(packet[8:12], uint32(packetType))
	copy(packet[12:12+len(payload)], payload)
	// The final two bytes are already zero and terminate the payload.
	return writeFull(w, packet)
}

func writeFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func readPacket(r io.Reader) (id, packetType int32, payload []byte, err error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, 0, nil, err
	}
	length := int32(binary.LittleEndian.Uint32(header[:]))
	if length < 10 {
		return 0, 0, nil, fmt.Errorf("%w: invalid packet length %d", ErrProtocol, length)
	}
	if length > maxPacketSize {
		return 0, 0, nil, fmt.Errorf("%w: packet length %d exceeds %d", ErrPacketTooLarge, length, maxPacketSize)
	}
	body := make([]byte, int(length))
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, 0, nil, fmt.Errorf("%w: truncated packet: %v", ErrProtocol, err)
	}
	if body[len(body)-2] != 0 || body[len(body)-1] != 0 {
		return 0, 0, nil, fmt.Errorf("%w: missing packet terminator", ErrProtocol)
	}
	id = int32(binary.LittleEndian.Uint32(body[0:4]))
	packetType = int32(binary.LittleEndian.Uint32(body[4:8]))
	payload = body[8 : len(body)-2]
	return id, packetType, payload, nil
}

// LoadPassword reads an RCON password file. Only a trailing LF or CRLF is
// removed; spaces and other characters are preserved. Errors never include the
// file contents.
func LoadPassword(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read rcon password file: %w", err)
	}
	password := string(data)
	switch {
	case strings.HasSuffix(password, "\r\n"):
		password = password[:len(password)-2]
	case strings.HasSuffix(password, "\n"):
		password = password[:len(password)-1]
	}
	if password == "" {
		return "", ErrEmptyPassword
	}
	return password, nil
}
