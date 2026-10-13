package discovery

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"
)

var (
	onvifGroup = net.IPv4(239, 255, 255, 250) // WS-Discovery / SSDP / SADP
	dahuaGroup = net.IPv4(239, 255, 255, 251)
	sadpGroup  = onvifGroup
)

// lanInterfaces returns up, non-loopback IPv4 interfaces.
func lanInterfaces() []net.Interface {
	ifs, _ := net.Interfaces()
	var out []net.Interface
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				out = append(out, ifi)
				break
			}
		}
	}
	return out
}

// interfaceBroadcasts returns the directed broadcast address of every LAN
// interface (255.255.255.255 only leaves through the default interface).
func interfaceBroadcasts() []string {
	var out []string
	for _, ifi := range lanInterfaces() {
		if ifi.Flags&net.FlagBroadcast == 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.To4() == nil {
				continue
			}
			ip, mask := n.IP.To4(), n.Mask
			if len(mask) == 16 {
				mask = mask[12:]
			}
			b := make(net.IP, 4)
			for i := range b {
				b[i] = ip[i] | ^mask[i]
			}
			if !b.Equal(ip) {
				out = append(out, b.String())
			}
		}
	}
	return out
}

// openSockets opens the sockets of one search: per LAN interface a socket
// bound to the wildcard address (port bindPort, SO_REUSEADDR) with the
// interface as multicast egress. A fixed port that cannot be bound falls
// back to an ephemeral one (unicast replies still arrive).
func openSockets(t *Targets, group net.IP, bindPort int) ([]*net.UDPConn, error) {
	if !t.Interfaces {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			return nil, err
		}
		return []*net.UDPConn{c}, nil
	}
	if group == nil {
		group = onvifGroup
	}
	var out []*net.UDPConn
	var lastErr error
	for _, ifi := range lanInterfaces() {
		ifi := ifi
		c, err := net.ListenMulticastUDP("udp4", &ifi, &net.UDPAddr{IP: group, Port: bindPort})
		if err != nil && bindPort != 0 {
			c, err = net.ListenMulticastUDP("udp4", &ifi, &net.UDPAddr{IP: group})
		}
		if err != nil {
			lastErr = err
			continue
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		// no multicast capable interface: a plain socket still covers unicast
		c, err := net.ListenUDP("udp4", &net.UDPAddr{})
		if err != nil {
			return nil, errors.Join(lastErr, err)
		}
		out = append(out, c)
	}
	return out, nil
}

func isGroupOrBroadcast(a *net.UDPAddr) bool {
	return a.IP.IsMulticast() || a.IP.Equal(net.IPv4bcast) || (len(a.IP.To4()) == 4 && a.IP.To4()[3] == 255)
}

// udpSearch sends the probes (twice) to all targets and hands every answer
// to handle until the timeout.
func udpSearch(ctx context.Context, t *Targets, timeout time.Duration, group net.IP, bindPort int, targets []string, probes [][]byte, handle func(netip.Addr, []byte)) error {
	if len(targets) == 0 {
		return nil
	}
	conns, err := openSockets(t, group, bindPort)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	var dsts []*net.UDPAddr
	for _, s := range targets {
		if a, err := net.ResolveUDPAddr("udp4", s); err == nil {
			dsts = append(dsts, a)
		}
	}
	send := func() {
		for i, c := range conns {
			for _, d := range dsts {
				// unicast targets once (routing picks the interface), groups
				// and broadcasts from every interface socket
				if i > 0 && !isGroupOrBroadcast(d) {
					continue
				}
				for _, p := range probes {
					_, _ = c.WriteToUDP(p, d)
				}
			}
		}
	}
	var wg sync.WaitGroup
	for _, c := range conns {
		c := c
		_ = c.SetReadDeadline(deadline)
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 65536)
			for {
				n, src, err := c.ReadFromUDPAddrPort(buf)
				if err != nil {
					return
				}
				handle(src.Addr().Unmap(), append([]byte(nil), buf[:n]...))
			}
		}()
	}
	send()
	retry := time.NewTimer(900 * time.Millisecond)
	select {
	case <-retry.C:
		send()
	case <-ctx.Done():
		retry.Stop()
	}
	stop := context.AfterFunc(ctx, func() {
		for _, c := range conns {
			_ = c.SetReadDeadline(time.Now())
		}
	})
	wg.Wait()
	stop()
	for _, c := range conns {
		c.Close()
	}
	return nil
}
