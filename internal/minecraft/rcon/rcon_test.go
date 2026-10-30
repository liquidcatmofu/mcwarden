package rcon

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liquidcatmofu/mcwarden/internal/minecraft"
)

const testPassword = "correct horse battery staple"

func writePasswordFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rcon-password")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testPasswordFile(t *testing.T) string {
	t.Helper()
	return writePasswordFile(t, testPassword+"\n")
}

func startFakeServer(t *testing.T, handler func(net.Conn) error) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if err := handler(conn); err != nil {
			t.Errorf("fake rcon server: %v", err)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("fake rcon server did not stop")
		}
	})
	return listener.Addr().String()
}

func readTestPacket(t *testing.T, conn net.Conn) (id, packetType int32, payload []byte, err error) {
	t.Helper()
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return 0, 0, nil, err
	}
	length := int32(binary.LittleEndian.Uint32(header[:]))
	if length < 10 || length > maxPacketSize {
		return 0, 0, nil, fmt.Errorf("test server: invalid packet length %d", length)
	}
	body := make([]byte, int(length))
	if _, err := io.ReadFull(conn, body); err != nil {
		return 0, 0, nil, err
	}
	return int32(binary.LittleEndian.Uint32(body[0:4])),
		int32(binary.LittleEndian.Uint32(body[4:8])),
		body[8 : len(body)-2], nil
}

func writeTestPacket(conn net.Conn, id, packetType int32, payload []byte) error {
	length := 4 + 4 + len(payload) + 2
	packet := make([]byte, 4+length)
	binary.LittleEndian.PutUint32(packet[0:4], uint32(length))
	binary.LittleEndian.PutUint32(packet[4:8], uint32(id))
	binary.LittleEndian.PutUint32(packet[8:12], uint32(packetType))
	copy(packet[12:], payload)
	_, err := conn.Write(packet)
	return err
}

func writeTestLength(conn net.Conn, length int32) error {
	var header [4]byte
	binary.LittleEndian.PutUint32(header[:], uint32(length))
	_, err := conn.Write(header[:])
	return err
}

func reportPasswordSafe(t *testing.T, context string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected an error", context)
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Fatalf("%s: error text contains the password", context)
	}
	t.Fatalf("%s: %v", context, err)
}

func TestCommandAuthenticatesAndSendsPacket(t *testing.T) {
	commands := make(chan string, 1)
	address := startFakeServer(t, func(conn net.Conn) error {
		id, packetType, payload, err := readTestPacket(t, conn)
		if err != nil {
			return err
		}
		if id != authRequestID || packetType != typeAuth {
			return fmt.Errorf("unexpected auth packet id=%d type=%d", id, packetType)
		}
		if string(payload) != testPassword {
			return errors.New("auth password mismatch")
		}
		if err := writeTestPacket(conn, id, typeAuthResponse, nil); err != nil {
			return err
		}
		id, packetType, payload, err = readTestPacket(t, conn)
		if err != nil {
			return err
		}
		if id != commandRequestID || packetType != typeCommand {
			return fmt.Errorf("unexpected command packet id=%d type=%d", id, packetType)
		}
		commands <- string(payload)
		return nil
	})

	client := New(address, testPasswordFile(t))
	if err := client.Command(context.Background(), "stop"); err != nil {
		reportPasswordSafe(t, "command", err)
	}
	select {
	case got := <-commands:
		if got != "stop" {
			t.Fatalf("command=%q want stop", got)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not receive the command packet")
	}
}

func TestCapabilities(t *testing.T) {
	client := New("", testPasswordFile(t))
	caps, err := client.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps != (minecraft.Capabilities{Command: true, Readiness: false}) {
		t.Fatalf("capabilities=%+v", caps)
	}
}

func TestClientFormattingDoesNotExposePassword(t *testing.T) {
	client := New("127.0.0.1:25575", testPasswordFile(t))
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if text := fmt.Sprintf(format, client); strings.Contains(text, testPassword) {
			t.Fatalf("%s formatting exposed the password", format)
		}
	}
}

func TestAuthenticationFailure(t *testing.T) {
	address := startFakeServer(t, func(conn net.Conn) error {
		if _, _, _, err := readTestPacket(t, conn); err != nil {
			return err
		}
		return writeTestPacket(conn, -1, typeAuthResponse, nil)
	})
	err := New(address, testPasswordFile(t)).Command(context.Background(), "stop")
	if !errors.Is(err, ErrAuthentication) {
		reportPasswordSafe(t, "authentication error", err)
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Fatal("authentication error contains the password")
	}
}

func TestContextCanceledBeforeDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := New("127.0.0.1:1", testPasswordFile(t)).Command(ctx, "stop")
	if !errors.Is(err, context.Canceled) {
		reportPasswordSafe(t, "canceled command", err)
	}
}

func TestContextCanceledDuringAuth(t *testing.T) {
	authSeen := make(chan struct{})
	address := startFakeServer(t, func(conn net.Conn) error {
		if _, _, _, err := readTestPacket(t, conn); err != nil {
			return err
		}
		close(authSeen)
		_, _ = io.Copy(io.Discard, conn)
		return nil
	})
	passwordFile := testPasswordFile(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- New(address, passwordFile).Command(ctx, "stop")
	}()
	select {
	case <-authSeen:
	case <-time.After(time.Second):
		t.Fatal("server did not receive the auth packet")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			reportPasswordSafe(t, "canceled during auth", err)
		}
	case <-time.After(time.Second):
		t.Fatal("command did not stop after cancellation")
	}
}

func TestCommandPasswordFileErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	err := New("127.0.0.1:1", missing).Command(context.Background(), "stop")
	if !errors.Is(err, os.ErrNotExist) {
		reportPasswordSafe(t, "missing password file", err)
	}

	empty := writePasswordFile(t, "\n")
	err = New("127.0.0.1:1", empty).Command(context.Background(), "stop")
	if !errors.Is(err, ErrEmptyPassword) {
		reportPasswordSafe(t, "empty password file", err)
	}
}

func TestCommandReadsPasswordFileLazily(t *testing.T) {
	commands := make(chan string, 1)
	address := startFakeServer(t, func(conn net.Conn) error {
		id, _, payload, err := readTestPacket(t, conn)
		if err != nil {
			return err
		}
		if string(payload) != testPassword {
			return errors.New("auth password mismatch")
		}
		if err := writeTestPacket(conn, id, typeAuthResponse, nil); err != nil {
			return err
		}
		_, _, payload, err = readTestPacket(t, conn)
		if err != nil {
			return err
		}
		commands <- string(payload)
		return nil
	})
	passwordFile := filepath.Join(t.TempDir(), "rcon-password")
	client := New(address, passwordFile)
	if err := os.WriteFile(passwordFile, []byte(testPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.Command(context.Background(), "stop"); err != nil {
		reportPasswordSafe(t, "lazy password read", err)
	}
	select {
	case got := <-commands:
		if got != "stop" {
			t.Fatalf("command=%q want stop", got)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not receive the command packet")
	}
}

func TestAuthResponseDeadline(t *testing.T) {
	address := startFakeServer(t, func(conn net.Conn) error {
		if _, _, _, err := readTestPacket(t, conn); err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, conn)
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := New(address, testPasswordFile(t)).Command(ctx, "stop")
	if !errors.Is(err, context.DeadlineExceeded) {
		reportPasswordSafe(t, "deadline", err)
	}
}

func TestMalformedPacket(t *testing.T) {
	for _, tt := range []struct {
		name   string
		length int32
		body   []byte
		want   error
	}{
		{name: "too short", length: 9, want: ErrProtocol},
		{name: "zero length", length: 0, want: ErrProtocol},
		{name: "negative length", length: -1, want: ErrProtocol},
		{name: "oversized length", length: maxPacketSize + 1, want: ErrPacketTooLarge},
		{name: "truncated body", length: 20, body: []byte{1, 2, 3}, want: ErrProtocol},
		{name: "missing terminator", length: 10, body: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, want: ErrProtocol},
	} {
		t.Run(tt.name, func(t *testing.T) {
			address := startFakeServer(t, func(conn net.Conn) error {
				if _, _, _, err := readTestPacket(t, conn); err != nil {
					return err
				}
				if err := writeTestLength(conn, tt.length); err != nil {
					return err
				}
				if len(tt.body) > 0 {
					if _, err := conn.Write(tt.body); err != nil {
						return err
					}
				}
				return nil
			})
			err := New(address, testPasswordFile(t)).Command(context.Background(), "stop")
			if !errors.Is(err, tt.want) {
				reportPasswordSafe(t, tt.name, err)
			}
		})
	}
}

func TestServerDisconnect(t *testing.T) {
	address := startFakeServer(t, func(conn net.Conn) error {
		_, _, _, err := readTestPacket(t, conn)
		return err
	})
	err := New(address, testPasswordFile(t)).Command(context.Background(), "stop")
	if err == nil {
		t.Fatal("expected a disconnect error")
	}
	if errors.Is(err, ErrAuthentication) {
		t.Fatal("disconnect was misreported as authentication failure")
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Fatal("disconnect error contains the password")
	}
}

func TestLoadPassword(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
		want    string
		wantErr error
	}{
		{name: "lf", content: "secret\n", want: "secret"},
		{name: "crlf", content: "secret\r\n", want: "secret"},
		{name: "no newline", content: "secret", want: "secret"},
		{name: "spaces preserved", content: " secret \n", want: " secret "},
		{name: "lone cr preserved", content: "secret\r", want: "secret\r"},
		{name: "only one newline removed", content: "secret\n\n", want: "secret\n"},
		{name: "empty", content: "", wantErr: ErrEmptyPassword},
		{name: "newline only", content: "\n", wantErr: ErrEmptyPassword},
		{name: "crlf only", content: "\r\n", wantErr: ErrEmptyPassword},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "password")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadPassword(path)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err=%v want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && got != tt.want {
				t.Fatal("password did not match the expected value")
			}
		})
	}
}

func TestLoadPasswordMissingFile(t *testing.T) {
	_, err := LoadPassword(filepath.Join(t.TempDir(), "absent"))
	if err == nil || errors.Is(err, ErrEmptyPassword) {
		t.Fatalf("err=%v", err)
	}
}
