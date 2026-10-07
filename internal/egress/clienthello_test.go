package egress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func gatewayCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), DNSNames: []string{"api.openai.com", "registry.npmjs.org", "unlisted.example"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}, roots
}

func tlsRecord(payload []byte) []byte {
	header := []byte{22, 3, 1, 0, 0}
	binary.BigEndian.PutUint16(header[3:], uint16(len(payload)))
	return append(header, payload...)
}

func tlsExtension(kind uint16, value []byte) []byte {
	header := make([]byte, 4)
	binary.BigEndian.PutUint16(header, kind)
	binary.BigEndian.PutUint16(header[2:], uint16(len(value)))
	return append(header, value...)
}

func serverNameExtension(host string) []byte {
	value := make([]byte, 5)
	binary.BigEndian.PutUint16(value, uint16(3+len(host)))
	binary.BigEndian.PutUint16(value[3:], uint16(len(host)))
	return tlsExtension(0, append(value, host...))
}

func clientHelloMessage(extensions []byte) []byte {
	body := make([]byte, 34) // version and random
	body[0], body[1] = 3, 3
	body = append(body, 0, 0, 2, 0x13, 1, 1, 0) // session, cipher suite, null compression
	body = binary.BigEndian.AppendUint16(body, uint16(len(extensions)))
	body = append(body, extensions...)
	header := []byte{1, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}
	return append(header, body...)
}

func TestClientHelloRecords(t *testing.T) {
	hello := clientHelloMessage(serverNameExtension("API.OPENAI.COM"))
	for _, size := range []int{1, 2, 3, 17, len(hello)} {
		var records []byte
		for offset := 0; offset < len(hello); offset += size {
			records = append(records, tlsRecord(hello[offset:min(offset+size, len(hello))])...)
		}
		trailing := []byte{23, 3, 3, 0, 3, 9, 8, 7}
		reader := bufio.NewReader(bytes.NewReader(append(bytes.Clone(records), trailing...)))
		got, err := readClientHello(reader, "api.openai.com")
		if err != nil || !bytes.Equal(got, records) {
			t.Fatalf("record fragments of %d: replay differs, err %v", size, err)
		}
		rest, _ := io.ReadAll(reader)
		if !bytes.Equal(rest, trailing) {
			t.Fatalf("record fragments of %d: unread coalesced bytes = %x", size, rest)
		}
	}
	// A final record may contain bytes following the first handshake. Replay must not drop them either.
	record := tlsRecord(append(bytes.Clone(hello), 0, 0, 0, 0))
	got, err := readClientHello(bytes.NewReader(record), "api.openai.com")
	if err != nil || !bytes.Equal(got, record) {
		t.Fatalf("coalesced final record changed: %v", err)
	}
}

func FuzzClientHello(f *testing.F) {
	f.Add(tlsRecord(clientHelloMessage(serverNameExtension("api.openai.com"))))
	f.Add(tlsRecord(clientHelloMessage(serverNameExtension("unlisted.example"))))
	f.Add([]byte("GET / HTTP/1.1\r\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		replayed, err := readClientHello(bytes.NewReader(data), "api.openai.com")
		if err == nil && (len(replayed) == 0 || len(replayed) > maxClientHelloWire || !bytes.HasPrefix(data, replayed)) {
			t.Fatal("accepted ClientHello was changed or exceeded its wire budget")
		}
	})
}

func TestClientHelloRefusals(t *testing.T) {
	valid := serverNameExtension("api.openai.com")
	record := tlsRecord(clientHelloMessage(valid))
	for end := range len(record) {
		if _, err := readClientHello(bytes.NewReader(record[:end]), "api.openai.com"); err == nil {
			t.Fatalf("accepted truncated ClientHello at byte %d", end)
		}
	}
	// Unicode case folding must not turn a non-ASCII SNI (Kelvin sign) into an allowed ASCII name.
	if _, err := readClientHello(bytes.NewReader(tlsRecord(clientHelloMessage(serverNameExtension("api.K.com")))), "api.k.com"); err == nil {
		t.Fatal("non-ASCII SNI passed through Unicode case folding")
	}
	for name, extensions := range map[string][]byte{
		"different SNI":    serverNameExtension("unlisted.example"),
		"no SNI":           tlsExtension(16, []byte{0, 3, 2, 'h', '2'}),
		"empty SNI":        serverNameExtension(""),
		"IP SNI":           serverNameExtension("93.184.216.34"),
		"trailing dot":     serverNameExtension("api.openai.com."),
		"duplicate SNI":    append(bytes.Clone(valid), valid...),
		"ECH":              append(bytes.Clone(valid), tlsExtension(0xfe0d, []byte{0})...),
		"ECH GREASE":       append(bytes.Clone(valid), tlsExtension(0xfe0d, nil)...),
		"duplicate other":  append(append(bytes.Clone(valid), tlsExtension(42, nil)...), tlsExtension(42, nil)...),
		"truncated header": append(bytes.Clone(valid), 1),
		"truncated vector": append(bytes.Clone(valid), 0, 42, 0, 8, 1),
		"unknown name":     tlsExtension(0, []byte{0, 4, 1, 0, 1, 'x'}),
		"second name":      tlsExtension(0, []byte{0, 8, 0, 0, 1, 'a', 0, 0, 1, 'b'}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readClientHello(bytes.NewReader(tlsRecord(clientHelloMessage(extensions))), "api.openai.com")
			var refused helloRefusal
			if !errors.As(err, &refused) {
				t.Fatalf("accepted or misclassified invalid ClientHello: %v", err)
			}
		})
	}
	for name, input := range map[string][]byte{
		"plaintext":            []byte("GET / HTTP/1.1\r\n"),
		"wrong handshake":      tlsRecord([]byte{2, 0, 0, 0}),
		"zero record":          {22, 3, 1, 0, 0},
		"large record":         {22, 3, 1, 0x40, 1},
		"large handshake":      tlsRecord([]byte{1, 1, 0, 1}),
		"short body":           tlsRecord([]byte{1, 0, 0, 0}),
		"interleaved fragment": append(tlsRecord([]byte{1}), []byte{23, 3, 3, 0, 1, 0}...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readClientHello(bytes.NewReader(input), "api.openai.com"); err == nil {
				t.Fatal("accepted invalid initial TLS record")
			}
		})
	}
	// Excessive record overhead is bounded even when the declared handshake itself is within its limit.
	fragmented := tlsRecord([]byte{1, 0, 0xff, 0xfc})
	for len(fragmented) <= maxClientHelloWire {
		fragmented = append(fragmented, tlsRecord([]byte{0})...)
	}
	if _, err := readClientHello(bytes.NewReader(fragmented), "api.openai.com"); !errors.Is(err, largeHello) {
		t.Fatalf("fragmentation limit = %v", err)
	}
}

// fragmentHelloConn splits the real crypto/tls ClientHello across records and TCP writes. TLS's transcript hashes
// handshake bytes, so the same verified handshake must still complete through the gateway.
type fragmentHelloConn struct {
	net.Conn
	first bool
}

func (c *fragmentHelloConn) Write(p []byte) (int, error) {
	if c.first || len(p) < 5 || p[0] != 22 {
		return c.Conn.Write(p)
	}
	c.first = true
	n := int(binary.BigEndian.Uint16(p[3:]))
	if n > len(p)-5 {
		return 0, io.ErrShortWrite
	}
	hello := p[5 : 5+n]
	var fragmented []byte
	for offset := 0; offset < len(hello); offset += 13 {
		fragmented = append(fragmented, tlsRecord(hello[offset:min(offset+13, len(hello))])...)
	}
	fragmented = append(fragmented, p[5+n:]...)
	for len(fragmented) > 0 {
		n, err := c.Conn.Write(fragmented[:min(3, len(fragmented))])
		if err != nil {
			return 0, err
		}
		fragmented = fragmented[n:]
	}
	return len(p), nil
}

func TestGatewayVerifiedTLS(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(tls.VersionName(version), func(t *testing.T) {
			f := newGatewayFixture(t, "runner")
			status, raw := f.connect(t, "api.openai.com:443", proxyUser+":"+f.token)
			if status != http.StatusOK {
				t.Fatalf("CONNECT status = %d", status)
			}
			conn := tls.Client(&fragmentHelloConn{Conn: raw}, &tls.Config{
				ServerName: "api.openai.com", RootCAs: f.roots, MinVersion: version, MaxVersion: version,
			})
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if err := conn.Handshake(); err != nil || len(conn.ConnectionState().VerifiedChains) == 0 {
				t.Fatalf("verified fragmented handshake: %v", err)
			}
			if _, err := io.WriteString(conn, "ping"); err != nil {
				t.Fatal(err)
			}
			var reply [4]byte
			if _, err := io.ReadFull(conn, reply[:]); err != nil || string(reply[:]) != "ping" {
				t.Fatalf("TLS application echo = %q, %v", reply, err)
			}
		})
	}
}

func TestGatewayRejectsDifferentTLSIdentity(t *testing.T) {
	f := newGatewayFixture(t, "runner")
	// The synthetic shared endpoint has a trusted certificate for both names. It accepts the unlisted identity
	// directly, isolating gateway policy from certificate or virtual-host rejection by the endpoint.
	upstream, err := f.gateway.dial(context.Background(), netip.MustParseAddrPort("93.184.216.34:443"))
	if err != nil {
		t.Fatal(err)
	}
	direct := tls.Client(upstream, &tls.Config{ServerName: "unlisted.example", RootCAs: f.roots})
	if err := direct.Handshake(); err != nil || len(direct.ConnectionState().VerifiedChains) == 0 {
		t.Fatalf("direct unlisted TLS identity: %v", err)
	}
	direct.Close()
	_, raw := f.connect(t, "api.openai.com:443", proxyUser+":"+f.token)
	conn := tls.Client(raw, &tls.Config{ServerName: "unlisted.example", RootCAs: f.roots})
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err := conn.Handshake(); err == nil {
		t.Fatal("CONNECT reached the unlisted TLS identity")
	}
	summary := f.gateway.collect("octomus-test-runner")
	if summary.Denied["api.openai.com:443"].Count != 1 || len(summary.Allowed) != 0 {
		t.Fatalf("TLS refusal summary = %+v", summary)
	}
	if log := f.log.String(); !strings.Contains(log, "TLS server name does not match CONNECT host") {
		t.Fatalf("missing bounded refusal reason: %s", log)
	}
}

func TestGatewayInspectionForwardsNothingOnRefusal(t *testing.T) {
	for name, payload := range map[string][]byte{
		"plaintext":    []byte("GET / HTTP/1.1\r\nHost: unlisted.example\r\n\r\n"),
		"SNI mismatch": tlsRecord(clientHelloMessage(serverNameExtension("unlisted.example"))),
		"ECH":          tlsRecord(clientHelloMessage(append(serverNameExtension("api.openai.com"), tlsExtension(0xfe0d, []byte{0})...))),
	} {
		t.Run(name, func(t *testing.T) {
			received := make(chan []byte, 1)
			f := newGatewayFixture(t, "runner", func(g *Gateway) {
				g.dial = func(context.Context, netip.AddrPort) (net.Conn, error) {
					upstream, peer := net.Pipe()
					go func() {
						defer peer.Close()
						data, _ := io.ReadAll(peer)
						received <- data
					}()
					return upstream, nil
				}
			})
			_, conn := f.connect(t, "api.openai.com:443", proxyUser+":"+f.token)
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			_, _ = conn.Write(payload)
			if _, err := conn.Read(make([]byte, 1)); err == nil {
				t.Fatal("invalid tunnel remained open")
			}
			select {
			case data := <-received:
				if len(data) != 0 {
					t.Fatalf("forwarded %d bytes before TLS policy validation", len(data))
				}
			case <-time.After(time.Second):
				t.Fatal("upstream was not closed")
			}
		})
	}
}

func TestGatewayInspectionStopsWithLeaseOrGateway(t *testing.T) {
	for _, cause := range []string{"lease revoked", "gateway stopped"} {
		t.Run(cause, func(t *testing.T) {
			f := newGatewayFixture(t, "runner")
			_, conn := f.connect(t, "api.openai.com:443", proxyUser+":"+f.token)
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			// An incomplete record keeps inspection pending until the ownership lifetime ends.
			_, _ = conn.Write([]byte{22, 3, 1, 0, 64, 1})
			if cause == "lease revoked" {
				f.gateway.leases.Revoke(f.token)
				f.gateway.sweep()
			} else {
				f.gateway.stop()
			}
			if _, err := conn.Read(make([]byte, 1)); err == nil {
				t.Fatal("inspection survived its lease or gateway")
			} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("inspection waited for the client timeout")
			}
		})
	}
}

func TestGatewayInspectionDeadline(t *testing.T) {
	t.Parallel()
	f := newGatewayFixture(t, "runner")
	_, stalled := f.connect(t, "api.openai.com:443", proxyUser+":"+f.token)
	defer stalled.Close()
	_ = stalled.SetDeadline(time.Now().Add(clientHelloTimeout + 5*time.Second))
	if _, err := stalled.Read(make([]byte, 1)); err == nil {
		t.Fatal("ClientHello deadline did not close the tunnel")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("gateway relied on the client's deadline")
	}
	if log := f.log.String(); !strings.Contains(log, "TLS ClientHello deadline exceeded") {
		t.Fatalf("missing inspection deadline evidence: %s", log)
	}
}

func TestGatewayClearsInspectionDeadline(t *testing.T) {
	t.Parallel()
	f := newGatewayFixture(t, "runner")
	_, raw := f.connect(t, "api.openai.com:443", proxyUser+":"+f.token)
	conn := tls.Client(raw, &tls.Config{ServerName: "api.openai.com", RootCAs: f.roots})
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(clientHelloTimeout + 5*time.Second))
	if err := conn.Handshake(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(clientHelloTimeout + 100*time.Millisecond)
	_, err := io.WriteString(conn, "ping")
	var response [4]byte
	if err == nil {
		_, err = io.ReadFull(conn, response[:])
	}
	if err != nil || string(response[:]) != "ping" {
		t.Fatalf("valid TLS tunnel retained its inspection deadline: %q, %v", response, err)
	}
}
