package realtime

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"net"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

// tailSize is how many trailing bytes of the upstream stream are kept for close recovery.
const tailSize = 8 << 10

// oversizedControlText is gorilla's protocol error for a control frame longer than 125 bytes.
const oversizedControlText = "len > 125 for control"

// tailConn remembers the last bytes read from an upstream socket. Bailian ends sessions with
// close frames whose reason runs past the 123 bytes the protocol allows (its quota message is
// 176 bytes); gorilla rejects such a frame as a protocol error and the reason is lost. The
// tail lets the session report the real code and reason instead of a bare 1011.
type tailConn struct {
	net.Conn

	mu   sync.Mutex
	tail []byte
}

func (c *tailConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.mu.Lock()

		c.tail = append(c.tail, p[:n]...)
		if len(c.tail) > tailSize {
			c.tail = append([]byte(nil), c.tail[len(c.tail)-tailSize:]...)
		}

		c.mu.Unlock()
	}

	return n, err
}

// OversizedClose scans the tail for the latest server close frame with a 16-bit payload
// length and returns its code and reason.
func (c *tailConn) OversizedClose() (code int, reason string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	tail := c.tail

	// Frame layout: 0x88 (FIN + close), 0x7e (unmasked, 16-bit length), length, status code,
	// reason. Servers never mask.
	for i := len(tail) - 6; i >= 0; i-- {
		if tail[i] != 0x88 || tail[i+1] != 0x7e {
			continue
		}

		length := int(binary.BigEndian.Uint16(tail[i+2 : i+4]))
		if length < 2 {
			continue
		}

		payload := tail[i+4:]
		if len(payload) > length {
			payload = payload[:length]
		}

		if len(payload) < 2 {
			continue
		}

		code = int(binary.BigEndian.Uint16(payload[:2]))
		if code < 1000 || code > 4999 {
			continue
		}

		reason = string(payload[2:])
		if !utf8.ValidString(reason) {
			continue
		}

		return code, reason, true
	}

	return 0, "", false
}

// oversizedClose recovers the close code and reason behind gorilla's "len > 125 for control"
// read error, when the connection was dialed through installTailDial.
func oversizedClose(conn *websocket.Conn, err error) (code int, reason string, ok bool) {
	if conn == nil || err == nil || !strings.Contains(err.Error(), oversizedControlText) {
		return 0, "", false
	}

	tc, isTail := conn.UnderlyingConn().(*tailConn)
	if !isTail {
		return 0, "", false
	}

	return tc.OversizedClose()
}

// installTailDial makes the dialer wrap direct (unproxied) connections in a tailConn. TLS is
// then done here rather than by gorilla, because gorilla only hands the raw socket to
// NetDialContext and the tail has to sit above the TLS layer to see frames.
func installTailDial(dialer *websocket.Dialer) {
	rawDial := dialer.NetDialContext
	if rawDial == nil {
		rawDial = (&net.Dialer{}).DialContext
	}

	dialer.NetDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := rawDial(ctx, network, addr)
		if err != nil {
			return nil, err
		}

		return &tailConn{Conn: conn}, nil
	}

	dialer.NetDialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := rawDial(ctx, network, addr)
		if err != nil {
			return nil, err
		}

		cfg := dialer.TLSClientConfig
		if cfg == nil {
			cfg = &tls.Config{MinVersion: tls.VersionTLS12}
		}

		cfg = cfg.Clone()
		if cfg.ServerName == "" {
			host, _, splitErr := net.SplitHostPort(addr)
			if splitErr != nil {
				host = addr
			}

			cfg.ServerName = host
		}

		tlsConn := tls.Client(conn, cfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()

			return nil, err
		}

		return &tailConn{Conn: tlsConn}, nil
	}
}
