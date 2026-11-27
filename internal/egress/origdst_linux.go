package egress

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"golang.org/x/sys/unix"
)

// soOriginalDst is SO_ORIGINAL_DST from linux/netfilter_ipv4.h: the address a
// connection had before the firewall redirected it.
const soOriginalDst = 80

// OriginalDst returns where a TCP connection redirected by netfilter was going.
// For a client that connected to the proxy directly, the kernel answers with the
// proxy's own loopback address, which the platform ranges refuse: the proxy
// cannot be made to connect to itself.
func OriginalDst(c net.Conn) (netip.AddrPort, error) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return netip.AddrPort{}, errors.New("not a TCP connection")
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("raw connection: %w", err)
	}
	var dst netip.AddrPort
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		// The kernel answers with a sockaddr_in. GetsockoptIPv6Mreq is the x/sys
		// call that returns enough raw bytes to hold one.
		mreq, err := unix.GetsockoptIPv6Mreq(int(fd), unix.SOL_IP, soOriginalDst)
		if err != nil {
			sockErr = err
			return
		}
		b := mreq.Multiaddr
		dst = netip.AddrPortFrom(netip.AddrFrom4([4]byte{b[4], b[5], b[6], b[7]}), uint16(b[2])<<8|uint16(b[3]))
	}); err != nil {
		return netip.AddrPort{}, fmt.Errorf("raw connection control: %w", err)
	}
	if sockErr != nil {
		return netip.AddrPort{}, fmt.Errorf("SO_ORIGINAL_DST: %w", sockErr)
	}
	return dst, nil
}
