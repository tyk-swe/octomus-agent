package chat

// A minimal RFC 6455 WebSocket client: enough for Slack Socket Mode and
// the Discord Gateway, so the binary gains no new dependency. Only text
// messages are used; ping/pong, fragmentation and close are handled.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
const maxWSMessage = 4 << 20

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	mu   sync.Mutex // writes
}

func wsDial(ctx context.Context, rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	port := u.Port()
	var conn net.Conn
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	switch u.Scheme {
	case "wss":
		if port == "" {
			host = net.JoinHostPort(u.Hostname(), "443")
		}
		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}}
		conn, err = tlsDialer.DialContext(ctx, "tcp", host)
	case "ws":
		if port == "" {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
		conn, err = dialer.DialContext(ctx, "tcp", host)
	default:
		return nil, fmt.Errorf("unsupported websocket scheme %q", u.Scheme)
	}
	if err != nil {
		return nil, err
	}
	// The dial timeout no longer applies once TCP/TLS is up, so bound the
	// HTTP upgrade itself and honour cancellation while it runs.
	stopCancel := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopCancel()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	var keyRaw [16]byte
	if _, err := rand.Read(keyRaw[:]); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyRaw[:])
	path := u.RequestURI()
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, u.Host, key)
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != 101 {
		conn.Close()
		return nil, fmt.Errorf("websocket upgrade refused: %s", resp.Status)
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	if accept := resp.Header.Get("Sec-WebSocket-Accept"); accept != "" && accept != base64.StdEncoding.EncodeToString(sum[:]) {
		conn.Close()
		return nil, errors.New("websocket upgrade returned a bad accept key")
	}
	if err := ctx.Err(); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return &wsConn{conn: conn, br: br}, nil
}

// wsCloseError preserves the peer's close code: Discord's non-resumable
// codes (4007, 4009, …) must not be flattened into a plain EOF, or the
// client would keep Resuming a session the gateway has discarded.
type wsCloseError struct {
	Code   uint16
	Reason string
}

func (e *wsCloseError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("websocket closed with code %d: %s", e.Code, e.Reason)
	}
	return fmt.Sprintf("websocket closed with code %d", e.Code)
}

func (c *wsConn) Close() error { return c.conn.Close() }

func (c *wsConn) SetReadDeadline(t time.Time) { _ = c.conn.SetReadDeadline(t) }

const (
	wsText   = 0x1
	wsBinary = 0x2
	wsClose  = 0x8
	wsPing   = 0x9
	wsPong   = 0xA
)

// ReadMessage returns the next complete data message, answering pings.
func (c *wsConn) ReadMessage() ([]byte, error) {
	var message []byte
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case wsPing:
			if err := c.writeFrame(wsPong, payload); err != nil {
				return nil, err
			}
			continue
		case wsPong:
			continue
		case wsClose:
			closeErr := &wsCloseError{Code: 1005} // no status received
			if len(payload) >= 2 {
				closeErr.Code = binary.BigEndian.Uint16(payload[:2])
				closeErr.Reason = string(payload[2:])
			}
			return nil, closeErr
		case wsText, wsBinary, 0x0:
			message = append(message, payload...)
			if len(message) > maxWSMessage {
				return nil, errors.New("websocket message too large")
			}
			if fin {
				return message, nil
			}
		}
	}
}

func (c *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return false, 0, nil, err
	}
	fin = head[0]&0x80 != 0
	op = head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > maxWSMessage {
		return false, 0, nil, errors.New("websocket frame too large")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return fin, op, payload, nil
}

// WriteMessage sends one text message (client frames are masked per RFC).
func (c *wsConn) WriteMessage(data []byte) error { return c.writeFrame(wsText, data) }

func (c *wsConn) WriteJSON(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.WriteMessage(data)
}

func (c *wsConn) writeFrame(op byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	var head []byte
	head = append(head, 0x80|op)
	switch n := len(payload); {
	case n < 126:
		head = append(head, 0x80|byte(n))
	case n <= 0xFFFF:
		head = append(head, 0x80|126, byte(n>>8), byte(n))
	default:
		head = append(head, 0x80|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		head = append(head, ext[:]...)
	}
	head = append(head, mask[:]...)
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if _, err := c.conn.Write(head); err != nil {
		return err
	}
	_, err := c.conn.Write(masked)
	return err
}

// backoffDelay grows reconnect pauses from 1s toward a 60s cap with jitter.
func backoffDelay(attempt int) time.Duration {
	d := time.Second << min(attempt, 6)
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	var b [1]byte
	_, _ = rand.Read(b[:])
	return d + time.Duration(b[0]%50)*10*time.Millisecond
}
