package httpx

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"odm/internal/config"
)

// socksHandshake performs a SOCKS4a or SOCKS5 CONNECT, matching the settings
// keys observed in IDM (bUseSocks/SocksType/SocksHost/SocksPort/SocksUser/
// SocksPass/Socks5ProxyDNS, re/FINDINGS.md §4).
func socksHandshake(c net.Conn, p config.Proxy, host, addr string, timeout time.Duration) error {
	st := p.SocksType
	if st == 0 {
		st = 5
	}
	if st == 4 {
		return socks4(c, host, addr)
	}
	return socks5(c, p, host, addr)
}

func socks4(c net.Conn, host, addr string) error {
	// Resolve locally: SOCKS4a is used only when the host is not resolvable.
	ip := net.ParseIP(host)
	req := []byte{0x04, 0x01, 0x00, 0x50}
	if ip4 := ip.To4(); ip4 != nil {
		req = append(req, ip4...)
		req = append(req, 0x00)
	} else {
		req = append(req, 0x00, 0x00, 0x00, 0x01, 0x00)
		req = append(req, []byte(host)...)
		req = append(req, 0x00)
	}
	if _, err := c.Write(req); err != nil {
		return err
	}
	resp := make([]byte, 8)
	if _, err := io.ReadFull(c, resp); err != nil {
		return err
	}
	if resp[1] != 0x5a {
		return fmt.Errorf("socks server cannot connect to %s (code %d)", addr, resp[1])
	}
	return nil
}

func socks5(c net.Conn, p config.Proxy, host, addr string) error {
	greeting := []byte{0x05, 0x01, 0x00}
	if p.SocksUser != "" {
		greeting = []byte{0x05, 0x02, 0x00, 0x02}
	}
	if _, err := c.Write(greeting); err != nil {
		return err
	}
	sel := make([]byte, 2)
	if _, err := io.ReadFull(c, sel); err != nil {
		return err
	}
	if sel[0] != 0x05 {
		return fmt.Errorf("unknown socks type %d", sel[0])
	}
	switch sel[1] {
	case 0x00:
		// no auth
	case 0x02:
		u, pw := p.SocksUser, p.SocksPass
		buf := []byte{0x01, byte(len(u))}
		buf = append(buf, []byte(u)...)
		buf = append(buf, byte(len(pw)))
		buf = append(buf, []byte(pw)...)
		if _, err := c.Write(buf); err != nil {
			return err
		}
		ar := make([]byte, 2)
		if _, err := io.ReadFull(c, ar); err != nil {
			return err
		}
		if ar[1] != 0x00 {
			return fmt.Errorf("cannot connect via socks server, authorization failed")
		}
	default:
		return fmt.Errorf("cannot connect via socks server, authorization required")
	}

	var dst []byte
	if p.SocksResolve {
		hb := []byte(host)
		if len(hb) > 255 {
			return fmt.Errorf("hostname too long for socks5")
		}
		dst = append([]byte{0x03, byte(len(hb))}, hb...)
	} else if ip4 := net.ParseIP(host).To4(); ip4 != nil {
		dst = append([]byte{0x01}, ip4...)
	} else if ip16 := net.ParseIP(host).To16(); ip16 != nil {
		dst = append([]byte{0x04}, ip16...)
	} else {
		hb := []byte(host)
		dst = append([]byte{0x03, byte(len(hb))}, hb...)
	}
	req := []byte{0x05, 0x01, 0x00}
	req = append(req, dst...)
	req = append(req, 0x00, 0x50)
	if _, err := c.Write(req); err != nil {
		return err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(c, head); err != nil {
		return err
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks server cannot connect to %s (code %d)", addr, head[1])
	}
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4 + 2
	case 0x04:
		skip = 16 + 2
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return err
		}
		skip = int(l[0]) + 2
	default:
		return fmt.Errorf("socks5: unknown address type %d", head[3])
	}
	if _, err := io.ReadFull(c, make([]byte, skip)); err != nil {
		return err
	}
	return nil
}

// ListenPort is a small helper for reserving a loopback port.
func ListenPort(network, addr string) (net.Listener, int, error) {
	l, err := net.Listen(network, addr)
	if err != nil {
		return nil, 0, err
	}
	tcp, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return l, 0, nil
	}
	return l, tcp.Port, nil
}

var _ = binary.BigEndian
var _ = strconv.Itoa
