package egress

import (
	"encoding/binary"
	"io"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

const (
	clientHelloTimeout = 10 * time.Second
	maxClientHello     = 64 << 10
	maxClientHelloWire = 128 << 10
	maxTLSPlaintext    = 1 << 14
)

type helloRefusal string

func (e helloRefusal) Error() string { return string(e) }

const (
	malformedHello helloRefusal = "malformed TLS ClientHello"
	largeHello     helloRefusal = "TLS ClientHello exceeds the inspection limit"
)

// readClientHello checks the first plaintext handshake without terminating TLS. Record and handshake boundaries
// are independent (RFC 8446, section 5.1); retain the exact records, including any bytes after the ClientHello in
// its final record, for replay after validation. Nothing read here has reached the upstream yet.
func readClientHello(r io.Reader, host string) ([]byte, error) {
	var records, message []byte
	want := 4
	for len(message) < want {
		var header [5]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			return nil, err
		}
		if header[0] != 22 || header[1] != 3 || header[2] < 1 || header[2] > 3 {
			return nil, helloRefusal("a plaintext TLS ClientHello is required")
		}
		n := int(binary.BigEndian.Uint16(header[3:]))
		if n == 0 || n > maxTLSPlaintext {
			return nil, malformedHello
		}
		if len(records)+len(header)+n > maxClientHelloWire {
			return nil, largeHello
		}
		records = append(records, header[:]...)
		start := len(records)
		records = append(records, make([]byte, n)...)
		if _, err := io.ReadFull(r, records[start:]); err != nil {
			return nil, err
		}
		message = append(message, records[start:]...)
		if len(message) >= 4 {
			if message[0] != 1 {
				return nil, helloRefusal("the first TLS handshake must be a ClientHello")
			}
			n = int(message[1])<<16 | int(message[2])<<8 | int(message[3])
			if n > maxClientHello-4 {
				return nil, largeHello
			}
			want = 4 + n
		}
	}
	if err := checkClientHello(message[4:want], host); err != nil {
		return nil, err
	}
	return records, nil
}

// tlsVector consumes a TLS vector with a one- or two-byte length, without allocating from an untrusted length.
func tlsVector(data *[]byte, width int) ([]byte, bool) {
	if len(*data) < width {
		return nil, false
	}
	n := int((*data)[0])
	if width == 2 {
		n = int(binary.BigEndian.Uint16(*data))
	}
	*data = (*data)[width:]
	if len(*data) < n {
		return nil, false
	}
	value := (*data)[:n]
	*data = (*data)[n:]
	return value, true
}

func checkClientHello(data []byte, host string) error {
	// legacy_version, random, session ID, cipher suites, compression methods, extensions.
	if len(data) < 34 || data[0] != 3 || data[1] < 1 || data[1] > 3 {
		return malformedHello
	}
	data = data[34:]
	session, ok := tlsVector(&data, 1)
	if !ok || len(session) > 32 {
		return malformedHello
	}
	ciphers, ok := tlsVector(&data, 2)
	if !ok || len(ciphers) < 2 || len(ciphers)%2 != 0 {
		return malformedHello
	}
	compression, ok := tlsVector(&data, 1)
	if !ok || len(compression) == 0 {
		return malformedHello
	}
	extensions, ok := tlsVector(&data, 2)
	if !ok || len(data) != 0 {
		return malformedHello
	}
	// A fixed bitset bounds duplicate-extension detection independently of the number of tiny extensions.
	var seen [8192]byte
	foundName := false
	for len(extensions) > 0 {
		if len(extensions) < 4 {
			return malformedHello
		}
		kind := binary.BigEndian.Uint16(extensions)
		extensions = extensions[2:]
		value, ok := tlsVector(&extensions, 2)
		if !ok || seen[kind/8]&(1<<(kind%8)) != 0 {
			return malformedHello
		}
		seen[kind/8] |= 1 << (kind % 8)
		switch kind {
		case 0:
			if err := checkServerName(value, host); err != nil {
				return err
			}
			foundName = true
		case 0xfe0d:
			// RFC 9849: the outer SNI cannot vouch for the encrypted inner name. GREASE is refused too.
			return helloRefusal("encrypted ClientHello is not allowed")
		}
	}
	if !foundName {
		return helloRefusal("TLS ClientHello requires plaintext SNI")
	}
	return nil
}

func checkServerName(data []byte, host string) error {
	names, ok := tlsVector(&data, 2)
	if !ok || len(data) != 0 || len(names) == 0 || names[0] != 0 {
		return malformedHello
	}
	names = names[1:]
	name, ok := tlsVector(&names, 2)
	if !ok || len(names) != 0 || len(name) == 0 || strings.HasSuffix(string(name), ".") {
		return malformedHello
	}
	// RFC 6066, section 3: one host_name, an ASCII DNS name without a trailing dot or IP literal.
	for _, c := range name {
		if c > 127 {
			return malformedHello
		}
	}
	normalized, err := wire.NormalizeHost(string(name))
	if err != nil {
		return malformedHello
	}
	if normalized != host {
		return helloRefusal("TLS server name does not match CONNECT host")
	}
	return nil
}
