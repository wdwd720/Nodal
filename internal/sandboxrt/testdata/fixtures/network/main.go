// Command network tries to exchange data with the outside world and reports
// each outcome.
//
// Go's wasip1 port carries an in-process fake network: net.Listen and a UDP
// "dial" succeed inside the module without touching the host, the way they
// would inside a process with no interfaces. So each attempt here is judged
// by whether anything actually crosses the boundary: a TCP connection
// established, a UDP answer received, an inbound connection accepted, a name
// resolved, an inherited socket used.
package main

import (
	"net"
	"os"
	"strconv"
	"time"

	nodalsdk "nodal.dev/sdk"
)

func main() {
	nodalsdk.Run(func(s *nodalsdk.Session) error {
		attempt := func(name string, err error) error {
			msg := ""
			if err != nil {
				msg = err.Error()
				if len(msg) > 200 {
					msg = msg[:200]
				}
			}
			return s.Log("attempt", map[string]string{"name": name, "ok": strconv.FormatBool(err == nil), "error": msg})
		}
		c, err := net.Dial("tcp", "1.1.1.1:443")
		if err == nil {
			_ = c.Close()
		}
		if e := attempt("dial_tcp", err); e != nil {
			return e
		}
		c, err = net.Dial("tcp", "example.com:80")
		if err == nil {
			_ = c.Close()
		}
		if e := attempt("dial_tcp_by_name", err); e != nil {
			return e
		}
		if e := attempt("udp_exchange", udpExchange()); e != nil {
			return e
		}
		if e := attempt("accept_inbound", acceptInbound()); e != nil {
			return e
		}
		_, err = net.LookupHost("example.com")
		if e := attempt("dns_lookup", err); e != nil {
			return e
		}
		fl, err := net.FileListener(os.NewFile(3, "socket"))
		if err == nil {
			_ = fl.Close()
		}
		return attempt("inherited_socket", err)
	})
}

// udpExchange sends a DNS query to a public resolver and waits for an answer.
func udpExchange() error {
	c, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	query := []byte{0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 3, 'c', 'o', 'm', 0, 0x00, 0x01, 0x00, 0x01}
	if _, err := c.Write(query); err != nil {
		return err
	}
	if err := c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	buf := make([]byte, 512)
	_, err = c.Read(buf)
	return err
}

// acceptInbound listens and waits for anyone outside to connect.
func acceptInbound() error {
	l, err := net.Listen("tcp", "0.0.0.0:8080")
	if err != nil {
		return err
	}
	defer func() { _ = l.Close() }()
	if tl, ok := l.(*net.TCPListener); ok {
		if err := tl.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			return err
		}
	}
	conn, err := l.Accept()
	if err == nil {
		_ = conn.Close()
	}
	return err
}
