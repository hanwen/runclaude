package proxy

// In-process DNS server used by runclaude inside the container netns. Serves
// A/AAAA queries: parses the question, resolves on the host via
// net.DefaultResolver, returns answers. Anything outside allowed returns
// NXDOMAIN. Anything other than A/AAAA returns NOTIMP. This is intentionally
// not a recursive resolver -- it is the container's only path to name
// resolution, and the proxy's allowlist controls egress anyway.

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"time"
)

func parseDNSQuery(buf []byte) (id uint16, name string, qtype uint16, err error) {
	if len(buf) < 12 {
		return 0, "", 0, fmt.Errorf("query too short")
	}
	id = binary.BigEndian.Uint16(buf[0:2])
	if qd := binary.BigEndian.Uint16(buf[4:6]); qd != 1 {
		return 0, "", 0, fmt.Errorf("qdcount %d != 1", qd)
	}
	pos := 12
	var parts []string
	for {
		if pos >= len(buf) {
			return 0, "", 0, fmt.Errorf("name truncated")
		}
		n := int(buf[pos])
		pos++
		if n == 0 {
			break
		}
		if n&0xc0 != 0 {
			return 0, "", 0, fmt.Errorf("unexpected compression in question")
		}
		if pos+n > len(buf) {
			return 0, "", 0, fmt.Errorf("label truncated")
		}
		parts = append(parts, string(buf[pos:pos+n]))
		pos += n
	}
	if pos+4 > len(buf) {
		return 0, "", 0, fmt.Errorf("qtype/qclass truncated")
	}
	qtype = binary.BigEndian.Uint16(buf[pos : pos+2])
	return id, strings.Join(parts, "."), qtype, nil
}

func questionEnd(buf []byte) int {
	pos := 12
	for buf[pos] != 0 {
		pos += 1 + int(buf[pos])
	}
	return pos + 1 + 4 // null label + qtype + qclass
}

func buildDNSAnswer(req []byte, ips []net.IP, qtype uint16) []byte {
	qend := questionEnd(req)
	out := make([]byte, 0, 512)
	out = append(out, req[:qend]...)
	binary.BigEndian.PutUint16(out[2:4], 0x8180)           // QR=1, RD=1, RA=1
	binary.BigEndian.PutUint16(out[6:8], uint16(len(ips))) // ANCOUNT
	for _, ip := range ips {
		var rdata []byte
		var rtype uint16
		if v4 := ip.To4(); v4 != nil && qtype == 1 {
			rdata, rtype = v4, 1
		} else if v6 := ip.To16(); v6 != nil && qtype == 28 && ip.To4() == nil {
			rdata, rtype = v6, 28
		} else {
			continue
		}
		out = append(out, 0xc0, 0x0c) // name = pointer to offset 12 (start of question name)
		out = binary.BigEndian.AppendUint16(out, rtype)
		out = binary.BigEndian.AppendUint16(out, 1) // class IN
		out = binary.BigEndian.AppendUint32(out, 60)
		out = binary.BigEndian.AppendUint16(out, uint16(len(rdata)))
		out = append(out, rdata...)
	}
	return out
}

func buildDNSError(req []byte, rcode uint16) []byte {
	out := make([]byte, len(req))
	copy(out, req)
	binary.BigEndian.PutUint16(out[2:4], 0x8180|rcode)
	return out
}

// HandleDNSQuery parses a single DNS request and returns the wire-format
// response. Hosts outside allowed receive NXDOMAIN; QTYPEs other than A/AAAA
// receive NOTIMP.
func HandleDNSQuery(buf []byte, allowed *Allowlist, logger *log.Logger) []byte {
	_, name, qtype, err := parseDNSQuery(buf)
	if err != nil {
		logger.Printf("dns: parse: %v", err)
		return buildDNSError(buf, 1) // FORMERR
	}
	name = strings.TrimSuffix(name, ".")
	if !allowed.Match(name) {
		logger.Printf("dns: deny %s", name)
		return buildDNSError(buf, 3) // NXDOMAIN
	}
	if qtype != 1 && qtype != 28 {
		logger.Printf("dns: notimp %s type=%d", name, qtype)
		return buildDNSError(buf, 4) // NOTIMP
	}
	network := "ip4"
	if qtype == 28 {
		network = "ip6"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, network, name)
	if err != nil {
		logger.Printf("dns: lookup %s %s: %v", network, name, err)
		return buildDNSError(buf, 2) // SERVFAIL
	}
	logger.Printf("dns: %s %s -> %v", network, name, ips)
	return buildDNSAnswer(buf, ips, qtype)
}

// ServeDNSUDP reads queries from conn and writes responses until conn is
// closed.
func ServeDNSUDP(conn net.PacketConn, allowed *Allowlist, logger *log.Logger) {
	defer conn.Close()
	buf := make([]byte, 1500)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		req := make([]byte, n)
		copy(req, buf[:n])
		go func() {
			resp := HandleDNSQuery(req, allowed, logger)
			if resp != nil {
				conn.WriteTo(resp, addr)
			}
		}()
	}
}

// ServeDNSTCP accepts DNS-over-TCP connections from ln and serves them until
// ln is closed.
func ServeDNSTCP(ln net.Listener, allowed *Allowlist, logger *log.Logger) {
	defer ln.Close()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			c.SetDeadline(time.Now().Add(10 * time.Second))
			var lb [2]byte
			if _, err := io.ReadFull(c, lb[:]); err != nil {
				return
			}
			msg := make([]byte, binary.BigEndian.Uint16(lb[:]))
			if _, err := io.ReadFull(c, msg); err != nil {
				return
			}
			resp := HandleDNSQuery(msg, allowed, logger)
			if resp == nil {
				return
			}
			binary.BigEndian.PutUint16(lb[:], uint16(len(resp)))
			c.Write(lb[:])
			c.Write(resp)
		}(c)
	}
}
