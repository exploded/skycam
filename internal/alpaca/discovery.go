package alpaca

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DiscoveryPort is the fixed ASCOM Alpaca discovery port.
const DiscoveryPort = 32227

// StartDiscovery answers ASCOM Alpaca UDP discovery broadcasts with this
// server's API port, so N.I.N.A. finds the device without manual setup.
//
// The socket binds with SO_REUSEADDR, as the Alpaca discovery spec directs
// for machines running several Alpaca servers: each hears the broadcast and
// replies with its own port, and the client queries every responder.
//
// A failed bind is a warning, not fatal: the device still works through
// its API port; only auto-discovery is lost. Returns when ctx is done.
func StartDiscovery(ctx context.Context, apiPort int) {
	addr := fmt.Sprintf("0.0.0.0:%d", DiscoveryPort)
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) { serr = setSocketReuse(fd) }); err != nil {
			return err
		}
		return serr
	}}
	conn, err := lc.ListenPacket(ctx, "udp", addr)
	if err != nil {
		slog.Warn("alpaca discovery: cannot bind; N.I.N.A. will not auto-discover skycam", "addr", addr, "err", err)
		return
	}
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	serveDiscovery(ctx, conn, apiPort)
}

// serveDiscovery is the read loop, split out for tests (which listen on a
// throwaway port instead of the real 32227).
func serveDiscovery(ctx context.Context, conn net.PacketConn, apiPort int) {
	defer conn.Close()
	local := &localNetworks{}
	slog.Info("alpaca discovery responder", "addr", conn.LocalAddr().String(), "alpaca_port", apiPort)
	reply := fmt.Sprintf("{\n\"AlpacaPort\":%d\n}", apiPort)

	buf := make([]byte, 1024)
	for {
		n, src, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("alpaca discovery read", "err", err)
			continue
		}
		// Every well-formed packet is answered: N.I.N.A. deliberately sends
		// several polls to survive a lost datagram, and the client dedupes
		// replies by responder endpoint.
		if !strings.HasPrefix(strings.TrimSpace(string(buf[:n])), "alpacadiscovery1") {
			continue
		}
		u, ok := src.(*net.UDPAddr)
		if !ok || !local.contains(u.IP) {
			continue // not from any of this machine's networks — off-LAN
		}
		// A probe from this machine's own LAN address is a local client
		// (N.I.N.A.) probing via the LAN interface; its loopback-interface
		// probe already gets an answer. Replying to both would list the
		// device twice — clients key responders by address, not UniqueID.
		if !u.IP.IsLoopback() && local.isSelf(u.IP) {
			continue
		}
		if _, err := conn.WriteTo([]byte(reply), src); err != nil {
			slog.Warn("alpaca discovery reply", "to", src.String(), "err", err)
		}
	}
}

// localNetworks answers whether an address is on one of this machine's own
// networks (same logic as alpaca-switch's responder). Interfaces are
// re-enumerated every 30 s so a reconnected adapter is picked up; if they
// cannot be enumerated at all it fails open — being over-permissive on a
// home LAN beats being undiscoverable.
type localNetworks struct {
	mu      sync.Mutex
	nets    []*net.IPNet
	ips     []net.IP // this machine's own addresses
	fetched time.Time
}

func (l *localNetworks) contains(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	nets := l.current()
	if len(nets) == 0 {
		return true
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// isSelf reports whether ip is one of this machine's own (non-loopback)
// interface addresses.
func (l *localNetworks) isSelf(ip net.IP) bool {
	l.current() // refresh the cache if stale
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, own := range l.ips {
		if own.Equal(ip) {
			return true
		}
	}
	return false
}

func (l *localNetworks) current() []*net.IPNet {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fetched.IsZero() || time.Since(l.fetched) > 30*time.Second {
		l.fetched = time.Now()
		if nets, ips := interfaceNetworks(); len(nets) > 0 {
			l.nets, l.ips = nets, ips
		}
	}
	return l.nets
}

func interfaceNetworks() ([]*net.IPNet, []net.IP) {
	ifaces, err := net.Interfaces()
	if err != nil {
		slog.Warn("alpaca discovery: enumerating interfaces", "err", err)
		return nil, nil
	}
	var nets []*net.IPNet
	var ips []net.IP
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP == nil || ipNet.Mask == nil {
				continue
			}
			nets = append(nets, &net.IPNet{IP: ipNet.IP.Mask(ipNet.Mask), Mask: ipNet.Mask})
			ips = append(ips, ipNet.IP)
		}
	}
	return nets, ips
}
