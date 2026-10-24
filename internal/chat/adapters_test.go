package chat

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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

func TestSlackBlocksDistinctActionIDsAndBoundedEscape(t *testing.T) {
	resp := Response{Text: "Confirm?", Buttons: []Button{
		{Label: "Confirm", Value: "confirm:abc"}, {Label: "Cancel", Value: "cancel:abc"},
	}}
	blocks := slackBlocks(resp)
	elements, ok := blocks[1]["elements"].([]map[string]any)
	if !ok || len(elements) != 2 {
		t.Fatalf("blocks = %+v; want two button elements", blocks)
	}
	if elements[0]["action_id"] == elements[1]["action_id"] {
		t.Fatalf("action_ids must differ within one block: %v", elements)
	}
	escaped := slackBlocks(Response{Text: strings.Repeat("&", 700)})
	section := escaped[0]["text"].(map[string]any)["text"].(string)
	if len([]rune(section)) > 3000 {
		t.Fatalf("escaped section is %d chars; Slack rejects over 3000", len([]rune(section)))
	}
}

func TestSlackTransientAPIErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := "internal_error"
		if strings.Contains(r.URL.Path, "permanent") {
			code = "channel_not_found"
		}
		fmt.Fprintf(w, `{"ok":false,"error":%q}`, code)
	}))
	defer srv.Close()
	a := newSlackAdapter(nil, &ProviderConfig{Provider: SlackProvider})
	err := a.postJSON(context.Background(), srv.URL+"/transient", "tok", map[string]any{"text": "x"})
	var apiErr *apiError
	if !errors.As(err, &apiErr) || !apiErr.retryable() {
		t.Fatalf("transient slack error = %v; want retryable apiError", err)
	}
	err = a.postJSON(context.Background(), srv.URL+"/permanent", "tok", map[string]any{"text": "x"})
	if !errors.As(err, &apiErr) || apiErr.retryable() {
		t.Fatalf("permanent slack error = %v; want non-retryable apiError", err)
	}
}

func TestDiscordComponentsSkipOversizeIDsAndDisableMentions(t *testing.T) {
	rows := discordComponents(Response{Buttons: []Button{
		{Label: "Too long", Value: strings.Repeat("x", 101)},
		{Label: "View task", Value: "task:view:abc-123"},
	}})
	if len(rows) != 1 || len(rows[0]["components"].([]map[string]any)) != 1 {
		t.Fatalf("components = %+v; want only the in-limit button", rows)
	}
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	a := newDiscordAdapter(nil, &ProviderConfig{Provider: DiscordProvider, BotToken: "tok"})
	a.apiBase = srv.URL
	if err := a.postMessage(context.Background(), "123", Response{Text: "hi <@999> @everyone"}); err != nil {
		t.Fatal(err)
	}
	mentions, ok := captured["allowed_mentions"].(map[string]any)
	if !ok {
		t.Fatalf("post body = %+v; want allowed_mentions", captured)
	}
	if parse, ok := mentions["parse"].([]any); !ok || len(parse) != 0 {
		t.Fatalf("allowed_mentions = %+v; want empty parse list", mentions)
	}
}

func TestDiscordUnauthorizedClickDoesNotEditAlert(t *testing.T) {
	svc, _ := testService(t)
	var mu sync.Mutex
	var callback map[string]any
	patched := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPatch {
			patched = true
		}
		if strings.Contains(r.URL.Path, "/callback") {
			_ = json.NewDecoder(r.Body).Decode(&callback)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	a := newDiscordAdapter(svc, &ProviderConfig{Provider: DiscordProvider, BotToken: "tok", AppID: "app-1"})
	a.apiBase = srv.URL
	raw := `{"id":"99","type":3,"token":"tok","channel_id":"C1","member":{"user":{"id":"STRANGER"}},
	  "data":{"custom_id":"task:view:abc-123"}}`
	a.handleInteraction(context.Background(), json.RawMessage(raw))
	time.Sleep(250 * time.Millisecond) // let any (wrong) async edit land
	mu.Lock()
	defer mu.Unlock()
	if callback == nil || callback["type"] != float64(4) {
		t.Fatalf("callback = %+v; want an ephemeral type-4 refusal, not a deferred update", callback)
	}
	if data, _ := callback["data"].(map[string]any); data == nil || data["flags"] != float64(64) {
		t.Fatalf("callback data = %+v; want ephemeral flags", callback["data"])
	}
	if patched {
		t.Fatal("unauthorized click edited the shared alert message")
	}
}

func wsHandshake(t *testing.T, ln net.Listener, then func(net.Conn)) {
	t.Helper()
	conn, err := ln.Accept()
	if err != nil {
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
	then(conn)
}

func TestWSCloseCodeSurfaced(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go wsHandshake(t, ln, func(conn net.Conn) {
		payload := make([]byte, 2)
		binary.BigEndian.PutUint16(payload, 4009)
		payload = append(payload, []byte("session timed out")...)
		frame := append([]byte{0x80 | wsClose, byte(len(payload))}, payload...)
		conn.Write(frame)
		time.Sleep(50 * time.Millisecond)
	})
	conn, err := wsDial(context.Background(), "ws://"+ln.Addr().String()+"/")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = conn.ReadMessage()
	if err == nil || !strings.Contains(err.Error(), "4009") {
		t.Fatalf("close error = %v; want the 4009 close code preserved", err)
	}
}

func TestWSDialHandshakeHonoursCancellation(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- conn
		buf := make([]byte, 512)
		_, _ = conn.Read(buf) // swallow the upgrade request, never reply
		time.Sleep(10 * time.Second)
		conn.Close()
	}()
	t.Cleanup(func() {
		select {
		case conn := <-accepted:
			conn.Close()
		default:
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := wsDial(ctx, "ws://"+ln.Addr().String()+"/")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled handshake unexpectedly succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wsDial ignored context cancellation during the upgrade handshake")
	}
}

func TestDiscordHeartbeatClosesOnMissedACK(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	closed := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 512)
		for {
			if _, err := conn.Read(buf); err != nil {
				close(closed)
				return
			}
		}
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ws := &wsConn{conn: client, br: bufio.NewReader(client)}
	a := &discordAdapter{hbAcked: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.heartbeat(ctx, ws, 20*time.Millisecond)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat kept a half-open connection alive after a missed ACK")
	}
}

func TestDiscordRegistrationRetriesTransientFailure(t *testing.T) {
	svc, _ := testService(t)
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	a := newDiscordAdapter(svc, &ProviderConfig{Provider: DiscordProvider, BotToken: "tok", AppID: "app-1"})
	a.apiBase = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a.registerLoop(ctx)
	if got := attempts.Load(); got != 2 {
		t.Fatalf("registration attempts = %d; want a transient failure then success", got)
	}
}

func TestTransportErrorsHideCapabilityURLs(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // every request now fails at the transport layer

	slack := newSlackAdapter(nil, &ProviderConfig{Provider: SlackProvider})
	err := slack.postJSON(context.Background(), deadURL+"/response/SECRET-RESPONSE-PATH", "", map[string]any{"text": "x"})
	if err == nil || strings.Contains(err.Error(), "SECRET-RESPONSE-PATH") {
		t.Fatalf("slack transport error = %v; want a failure without the response URL", err)
	}
	discord := newDiscordAdapter(nil, &ProviderConfig{Provider: DiscordProvider, BotToken: "tok"})
	discord.apiBase = deadURL
	err = discord.rest(context.Background(), http.MethodPost, "/interactions/1/SECRET-INTERACTION-TOKEN/callback", map[string]any{"type": 5}, false)
	if err == nil || strings.Contains(err.Error(), "SECRET-INTERACTION-TOKEN") {
		t.Fatalf("discord transport error = %v; want a failure without the interaction token", err)
	}
}

func readClientFrame(br *bufio.Reader) (string, error) {
	var head [2]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		return "", err
	}
	length := int(head[1] & 0x7F)
	if length == 126 {
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return "", err
		}
		length = int(binary.BigEndian.Uint16(ext[:]))
	}
	var mask [4]byte
	if _, err := io.ReadFull(br, mask[:]); err != nil {
		return "", err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil {
		return "", err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return string(payload), nil
}

func writeServerText(conn net.Conn, text string) {
	frame := []byte{0x80 | wsText}
	switch {
	case len(text) < 126:
		frame = append(frame, byte(len(text)))
	default:
		frame = append(frame, 126, byte(len(text)>>8), byte(len(text)))
	}
	conn.Write(append(frame, text...))
}

// The gateway read loop must answer an opcode-1 heartbeat request even
// while an interaction callback is still in flight at the REST layer.
func TestDiscordGatewayAnswersHeartbeatRequestDuringCallback(t *testing.T) {
	svc, _ := testService(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	callbackStarted := make(chan struct{}, 1)
	releaseCallback := make(chan struct{})
	rest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/gateway/bot"):
			fmt.Fprintf(w, `{"url":"ws://%s"}`, ln.Addr().String())
		case strings.Contains(r.URL.Path, "/callback"):
			select {
			case callbackStarted <- struct{}{}:
			default:
			}
			<-releaseCallback
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer rest.Close()
	released := false
	release := func() {
		if !released {
			released = true
			close(releaseCallback)
		}
	}
	defer release()

	a := newDiscordAdapter(svc, &ProviderConfig{Provider: DiscordProvider, BotToken: "tok", AppID: "app-1"})
	a.apiBase = rest.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.serveOnce(ctx)

	sendHeartbeatRequest := make(chan struct{}, 1)
	heartbeatSeen := make(chan string, 1)
	go wsHandshake(t, ln, func(conn net.Conn) {
		br := bufio.NewReader(conn)
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		writeServerText(conn, `{"op":10,"d":{"heartbeat_interval":60000}}`)
		if _, err := readClientFrame(br); err != nil { // Identify
			return
		}
		writeServerText(conn, `{"op":0,"s":1,"t":"INTERACTION_CREATE","d":{"id":"42","type":2,"token":"tok","channel_id":"C1","member":{"user":{"id":"D-OPERATOR"}},"data":{"name":"octomus","options":[{"name":"help","options":[]}]}}}`)
		<-sendHeartbeatRequest
		writeServerText(conn, `{"op":1,"d":null}`)
		frame, err := readClientFrame(br)
		if err == nil {
			heartbeatSeen <- frame
		}
		time.Sleep(100 * time.Millisecond)
	})

	select {
	case <-callbackStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("interaction callback never started")
	}
	sendHeartbeatRequest <- struct{}{}
	select {
	case frame := <-heartbeatSeen:
		if !strings.Contains(frame, `"op":1`) {
			t.Fatalf("frame after heartbeat request = %q; want an opcode-1 heartbeat", frame)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("gateway ignored the heartbeat request while the interaction callback was in flight")
	}
}

func TestSlackTextCannotMention(t *testing.T) {
	blocks := slackBlocks(Response{Text: "<!channel> deploy @here"})
	section := blocks[0]["text"].(map[string]any)
	if section["verbatim"] != true {
		t.Fatalf("section text = %+v; want verbatim so Slack never parses mentions", section)
	}
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()
	a := newSlackAdapter(nil, &ProviderConfig{Provider: SlackProvider, BotToken: "tok"})
	a.apiBase = srv.URL
	if err := a.postMessage(context.Background(), "C1", "", Response{Text: "<!channel> deploy"}); err != nil {
		t.Fatal(err)
	}
	if captured["mrkdwn"] != false {
		t.Fatalf("postMessage body = %+v; want mrkdwn disabled for the fallback text", captured)
	}
	captured = nil
	if err := a.postResponseURL(srv.URL+"/response", Response{Text: "<!channel> deploy"}, true); err != nil {
		t.Fatal(err)
	}
	if fallback, _ := captured["text"].(string); strings.Contains(fallback, "<!channel>") {
		t.Fatalf("response fallback = %q; want the broadcast syntax escaped", fallback)
	}
}

func TestNextDigestAcrossSpringForwardGap(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-03-08 skips 02:00–03:00 in New York. The next digest after
	// that morning belongs at the configured 02:30 on March 9.
	now := time.Date(2026, 3, 8, 8, 0, 0, 0, loc)
	got := nextDigest(now, loc, "02:30")
	want := time.Date(2026, 3, 9, 2, 30, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("nextDigest across the gap = %v; want %v", got, want)
	}
}

// An interaction whose REST acknowledgement is in flight when the gateway
// connection drops must still be answered: the gateway will not replay it
// on Resume, so the connection-lifetime context must not cancel it.
func TestDiscordInteractionSurvivesGatewayReconnect(t *testing.T) {
	svc, _ := testService(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	callbackStarted := make(chan struct{}, 1)
	releaseCallback := make(chan struct{})
	patchSeen := make(chan struct{}, 1)
	rest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/gateway/bot"):
			fmt.Fprintf(w, `{"url":"ws://%s"}`, ln.Addr().String())
		case strings.Contains(r.URL.Path, "/callback"):
			select {
			case callbackStarted <- struct{}{}:
			default:
			}
			<-releaseCallback
			w.WriteHeader(http.StatusNoContent)
		default:
			select {
			case patchSeen <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer rest.Close()
	released := false
	release := func() {
		if !released {
			released = true
			close(releaseCallback)
		}
	}
	defer release()

	a := newDiscordAdapter(svc, &ProviderConfig{Provider: DiscordProvider, BotToken: "tok", AppID: "app-1"})
	a.apiBase = rest.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- a.serveOnce(ctx) }()

	closeGateway := make(chan struct{}, 1)
	go wsHandshake(t, ln, func(conn net.Conn) {
		br := bufio.NewReader(conn)
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		writeServerText(conn, `{"op":10,"d":{"heartbeat_interval":60000}}`)
		if _, err := readClientFrame(br); err != nil { // Identify
			return
		}
		writeServerText(conn, `{"op":0,"s":1,"t":"INTERACTION_CREATE","d":{"id":"43","type":2,"token":"tok","channel_id":"C1","member":{"user":{"id":"D-OPERATOR"}},"data":{"name":"octomus","options":[{"name":"help","options":[]}]}}}`)
		<-closeGateway // returning closes the socket, dropping the connection
	})

	select {
	case <-callbackStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("interaction callback never started")
	}
	closeGateway <- struct{}{}
	select {
	case <-serveDone:
	case <-time.After(5 * time.Second):
		t.Fatal("serveOnce did not end after the gateway connection dropped")
	}
	release()
	select {
	case <-patchSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("interaction was dropped when the gateway reconnected; want its reply delivered")
	}
}

func TestSlackWebAPIRequiresPositiveAck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "chat.postMessage") {
			fmt.Fprint(w, `{"ok":tru`) // truncated acknowledgement
			return
		}
		fmt.Fprint(w, "ok") // response URLs answer in plain text
	}))
	defer srv.Close()
	a := newSlackAdapter(nil, &ProviderConfig{Provider: SlackProvider, BotToken: "tok"})
	a.apiBase = srv.URL
	if err := a.postMessage(context.Background(), "C1", "", Response{Text: "alert"}); err == nil {
		t.Fatal("undecodable Web API reply counted as delivered; want a retryable failure")
	}
	if err := a.postResponseURL(srv.URL+"/response", Response{Text: "reply"}, true); err != nil {
		t.Fatalf("plain-text response URL acknowledgement rejected: %v", err)
	}
}

func TestReconnectAttemptResetsAfterHealthyConnection(t *testing.T) {
	if got := reconnectAttempt(6, 2*time.Hour); got != 0 {
		t.Fatalf("attempt after a healthy connection = %d; want a reset streak", got)
	}
	if got := reconnectAttempt(2, time.Second); got != 3 {
		t.Fatalf("attempt after a quick failure = %d; want the streak to grow", got)
	}
	if d := backoffDelay(0); d < time.Second || d > 2*time.Second {
		t.Fatalf("backoff after reset = %v; want a prompt ~1s reconnect", d)
	}
}

func TestAnswerHeartbeatRequestPreservesACKState(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	received := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		frame, err := readClientFrame(bufio.NewReader(conn))
		if err == nil {
			received <- frame
		}
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ws := &wsConn{conn: client, br: bufio.NewReader(client)}
	defer ws.Close()
	a := &discordAdapter{hbAcked: true}
	if err := a.answerHeartbeatRequest(ws); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-received:
		if !strings.Contains(frame, `"op":1`) {
			t.Fatalf("requested heartbeat reply = %q; want opcode 1", frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("requested heartbeat was not answered")
	}
	a.mu.Lock()
	acked := a.hbAcked
	a.mu.Unlock()
	if !acked {
		t.Fatal("requested heartbeat clobbered the periodic ACK state")
	}
}

func TestDiscordTaskIDOptionIsOptional(t *testing.T) {
	var captured []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	a := newDiscordAdapter(nil, &ProviderConfig{Provider: DiscordProvider, BotToken: "tok", AppID: "app-1"})
	a.apiBase = srv.URL
	if err := a.registerCommands(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(captured) != 1 {
		t.Fatalf("registered %d commands; want one", len(captured))
	}
	subs, _ := captured[0]["options"].([]any)
	for _, raw := range subs {
		sub, _ := raw.(map[string]any)
		if sub["name"] != "task" {
			continue
		}
		opts, _ := sub["options"].([]any)
		if len(opts) == 0 {
			t.Fatal("task subcommand has no options")
		}
		id, _ := opts[0].(map[string]any)
		if id["name"] != "id" || id["required"] != false {
			t.Fatalf("task id option = %+v; want optional so remembered-task lookup is reachable", id)
		}
		return
	}
	t.Fatal("task subcommand not registered")
}

func TestDiscordInvalidSessionPreservesResumableSession(t *testing.T) {
	for _, tc := range []struct {
		name      string
		resumable bool
	}{
		{name: "resumable", resumable: true},
		{name: "not resumable", resumable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := testService(t)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() })

			a := newDiscordAdapter(svc, &ProviderConfig{Provider: DiscordProvider, BotToken: "tok", AppID: "app-1"})
			seq := int64(41)
			resumeURL := "ws://" + ln.Addr().String()
			a.sessionID = "session-123"
			a.resumeURL = resumeURL
			a.seq = &seq

			initialFrame := make(chan string, 1)
			go wsHandshake(t, ln, func(conn net.Conn) {
				br := bufio.NewReader(conn)
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				writeServerText(conn, `{"op":10,"d":{"heartbeat_interval":60000}}`)
				frame, err := readClientFrame(br)
				if err != nil {
					return
				}
				initialFrame <- frame
				writeServerText(conn, fmt.Sprintf(`{"op":9,"d":%t}`, tc.resumable))
			})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			serveDone := make(chan error, 1)
			go func() { serveDone <- a.serveOnce(ctx) }()

			select {
			case frame := <-initialFrame:
				if !strings.Contains(frame, `"op":6`) || !strings.Contains(frame, `"session_id":"session-123"`) {
					t.Fatalf("initial gateway frame = %q; want resume for the preset session", frame)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("gateway client did not resume the preset session")
			}
			select {
			case err := <-serveDone:
				if err == nil || !strings.Contains(err.Error(), "discord session invalid") {
					t.Fatalf("serveOnce error = %v; want invalid-session reconnect", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("serveOnce did not end after invalid session")
			}

			a.mu.Lock()
			gotSession, gotResumeURL, gotSeq := a.sessionID, a.resumeURL, a.seq
			a.mu.Unlock()
			if tc.resumable {
				if gotSession != "session-123" || gotResumeURL != resumeURL || gotSeq == nil || *gotSeq != 41 {
					t.Fatalf("session after resumable invalid-session = %q, %q, %v; want all preserved", gotSession, gotResumeURL, gotSeq)
				}
			} else if gotSession != "" || gotResumeURL != "" || gotSeq != nil {
				t.Fatalf("session after non-resumable invalid-session = %q, %q, %v; want all cleared", gotSession, gotResumeURL, gotSeq)
			}
		})
	}
}
