package chat

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDiscordCommandFlattening(t *testing.T) {
	var it discordInteraction
	raw := `{"id":"1","type":2,"token":"tok","channel_id":"C9","member":{"user":{"id":"D1"}},
	  "data":{"name":"octomus","options":[{"name":"task","options":[
	    {"name":"id","value":"abc-123"},{"name":"repo","value":"acme/widgets"}]}]}}`
	if err := json.Unmarshal([]byte(raw), &it); err != nil {
		t.Fatal(err)
	}
	text, ok := it.commandText()
	if !ok || text != "task abc-123 --repo acme/widgets" {
		t.Fatalf("commandText = %q, %v", text, ok)
	}
	if it.userID() != "D1" {
		t.Fatalf("userID = %q", it.userID())
	}
}

func TestSlackSlashDecode(t *testing.T) {
	a := newSlackAdapter(nil, &ProviderConfig{Provider: SlackProvider})
	payload, _ := json.Marshal(map[string]any{
		"command": "/octomus", "text": "status", "user_id": "U1",
		"channel_id": "C1", "response_url": "https://hooks.slack.com/x",
	})
	inv, ack, respond, ok := a.decode(slackEnvelope{EnvelopeID: "e1", Type: "slash_commands", Payload: payload})
	if !ok || inv.Text != "status" || inv.UserID != "U1" || inv.EventID != "e1" || ack == nil || respond == nil {
		t.Fatalf("decode = inv %+v ack %v ok %v", inv, ack, ok)
	}
	other, _ := json.Marshal(map[string]any{"command": "/other", "text": "x"})
	if _, _, _, ok := a.decode(slackEnvelope{Type: "slash_commands", Payload: other}); ok {
		t.Fatal("non-octomus slash command accepted")
	}
}

func TestSlackMentionDecode(t *testing.T) {
	a := newSlackAdapter(nil, &ProviderConfig{Provider: SlackProvider})
	payload, _ := json.Marshal(map[string]any{
		"event_id": "Ev1",
		"event":    map[string]any{"type": "app_mention", "user": "U9", "channel": "C9", "text": "<@UBOT> tasks 3", "ts": "12.34"},
	})
	inv, _, _, ok := a.decode(slackEnvelope{EnvelopeID: "e9", Type: "events_api", Payload: payload})
	if !ok || inv.Text != "tasks 3" || inv.EventID != "Ev1" || inv.ThreadID != "12.34" {
		t.Fatalf("mention decode = %+v ok %v", inv, ok)
	}
}

func TestNextDigest(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 24, 7, 30, 0, 0, loc)
	if got := nextDigest(now, loc, "08:00"); !got.Equal(time.Date(2026, 10, 24, 8, 0, 0, 0, loc)) {
		t.Fatalf("nextDigest before boundary = %v", got)
	}
	now = time.Date(2026, 10, 24, 8, 0, 0, 0, loc)
	if got := nextDigest(now, loc, "08:00"); !got.Equal(time.Date(2026, 10, 25, 8, 0, 0, 0, loc)) {
		t.Fatalf("nextDigest at boundary = %v", got)
	}
}

// wsTestServer is a minimal RFC 6455 peer: it accepts the upgrade, echoes
// one client message back split across two fragments, then expects a pong
// after its ping. It runs in its own goroutine, so it reports with Errorf.
func wsTestServer(t *testing.T, ln net.Listener) {
	t.Helper()
	conn, err := ln.Accept()
	if err != nil {
		t.Errorf("accept: %v", err)
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	var key string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "sec-websocket-key") {
			key = strings.TrimSpace(value)
		}
		if line == "\r\n" {
			break
		}
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		base64.StdEncoding.EncodeToString(sum[:]))
	readFrame := func() (byte, []byte, error) {
		var head [2]byte
		if _, err := io.ReadFull(br, head[:]); err != nil {
			return 0, nil, err
		}
		op := head[0] & 0x0F
		length := int(head[1] & 0x7F)
		if length == 126 {
			var ext [2]byte
			io.ReadFull(br, ext[:])
			length = int(binary.BigEndian.Uint16(ext[:]))
		}
		var mask [4]byte
		io.ReadFull(br, mask[:])
		payload := make([]byte, length)
		if _, err := io.ReadFull(br, payload); err != nil {
			return 0, nil, err
		}
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
		return op, payload, nil
	}
	writeFrame := func(fin bool, op byte, payload []byte) {
		first := op
		if fin {
			first |= 0x80
		}
		frame := []byte{first}
		switch {
		case len(payload) < 126:
			frame = append(frame, byte(len(payload)))
		default:
			frame = append(frame, 126, byte(len(payload)>>8), byte(len(payload)))
		}
		frame = append(frame, payload...)
		conn.Write(frame)
	}
	_, payload, err := readFrame()
	if err != nil {
		return
	}
	half := len(payload) / 2
	writeFrame(false, wsText, payload[:half])
	writeFrame(true, 0x0, payload[half:])
	writeFrame(true, wsPing, []byte("ping"))
	op, _, err := readFrame()
	if err != nil || op != wsPong {
		return
	}
	writeFrame(true, wsText, []byte("pong-ok"))
}

func TestWSConnRoundTrip(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go wsTestServer(t, ln)
	url := "ws://" + ln.Addr().String() + "/"
	conn, err := wsDial(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	sent := strings.Repeat("octomus-", 40) // >125 bytes: extended length both ways
	if err := conn.WriteMessage([]byte(sent)); err != nil {
		t.Fatal(err)
	}
	got, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != sent {
		t.Fatalf("fragmented echo = %q; want the full sent message", got)
	}
	got, err = conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "pong-ok" {
		t.Fatalf("after ping/pong = %q; want pong-ok", got)
	}
}
