package main

// Container-side network setup. The reusable proxy bits (CA, MITM server, DNS,
// bedrock signer) live in the proxy package; this file keeps the runclaude
// specifics: parsing --allow-domain, binding sockets inside the new netns,
// shipping listener fds back to the host process, and installing the nftables
// drop-by-default policy.

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// domainList tracks whether the user explicitly passed --allow-domain
// (possibly with an empty value) so we know not to fall back to defaults.
type domainList struct {
	items []string
	set   bool
}

func (d *domainList) String() string { return strings.Join(d.items, ",") }
func (d *domainList) Set(v string) error {
	d.set = true
	if v != "" {
		d.items = append(d.items, v)
	}
	return nil
}

func enforceAllowlist(cfg *Config) bool {
	return cfg.RestrictNet && (len(cfg.AllowedDomains) > 0 || len(cfg.MitmDomains) > 0)
}

// ---------- fd-passing helpers ----------

func sendFds(sock *os.File, fds []int) error {
	rights := syscall.UnixRights(fds...)
	// One dummy data byte: SCM_RIGHTS without payload is allowed but some
	// libcs require at least one byte. Stay portable.
	return syscall.Sendmsg(int(sock.Fd()), []byte{0}, rights, nil, 0)
}

func recvFds(sock *os.File, n int) ([]int, error) {
	oob := make([]byte, syscall.CmsgSpace(n*4))
	buf := make([]byte, 1)
	_, oobn, _, _, err := syscall.Recvmsg(int(sock.Fd()), buf, oob, 0)
	if err != nil {
		return nil, err
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, err
	}
	var fds []int
	for _, m := range msgs {
		fs, err := syscall.ParseUnixRights(&m)
		if err != nil {
			return nil, err
		}
		fds = append(fds, fs...)
	}
	if len(fds) != n {
		for _, fd := range fds {
			syscall.Close(fd)
		}
		return nil, fmt.Errorf("expected %d fds, got %d", n, len(fds))
	}
	return fds, nil
}

// setupNetwork brings up loopback, binds the proxy + DNS listener sockets
// inside this (container) netns, ships their fds back to mainErr via the
// fd-3 comm socket inherited through the exec chain, and installs an
// nftables policy that drops everything not on lo.  Returns the bound proxy
// port for HTTPS_PROXY env construction.
func setupNetwork(cfg *Config) (int, error) {
	if out, err := exec.Command("ip", "link", "set", "lo", "up").CombinedOutput(); err != nil {
		return 0, fmt.Errorf("ip link set lo up: %w: %s", err, out)
	}

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("listen proxy: %w", err)
	}
	port := proxyLn.Addr().(*net.TCPAddr).Port
	proxyFile, err := proxyLn.(*net.TCPListener).File()
	if err != nil {
		return 0, fmt.Errorf("listener fd: %w", err)
	}

	udp, err := net.ListenPacket("udp", "127.0.0.1:53")
	if err != nil {
		return 0, fmt.Errorf("listen dns udp: %w", err)
	}
	dnsUDPFile, err := udp.(*net.UDPConn).File()
	if err != nil {
		return 0, fmt.Errorf("udp fd: %w", err)
	}

	tcpDNS, err := net.Listen("tcp", "127.0.0.1:53")
	if err != nil {
		return 0, fmt.Errorf("listen dns tcp: %w", err)
	}
	dnsTCPFile, err := tcpDNS.(*net.TCPListener).File()
	if err != nil {
		return 0, fmt.Errorf("dns tcp fd: %w", err)
	}

	comm := os.NewFile(3, "comm")
	if comm == nil {
		return 0, fmt.Errorf("fd 3 (comm socket) not present")
	}
	if err := sendFds(comm, []int{int(proxyFile.Fd()), int(dnsUDPFile.Fd()), int(dnsTCPFile.Fd())}); err != nil {
		return 0, fmt.Errorf("sendFds: %w", err)
	}
	// We don't need these in-process anymore; the host side serves them.
	proxyLn.Close()
	udp.Close()
	tcpDNS.Close()
	proxyFile.Close()
	dnsUDPFile.Close()
	dnsTCPFile.Close()
	comm.Close()

	rules := `
		table inet runclaude {
			chain output {
				type filter hook output priority filter; policy drop;
				oif lo accept
			}
		}`
	nft := exec.Command("nft", "-f", "-")
	nft.Stdin = strings.NewReader(rules)
	if out, err := nft.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("nft: %w: %s", err, out)
	}
	return port, nil
}
