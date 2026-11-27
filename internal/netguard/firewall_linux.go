package netguard

import (
	"fmt"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// Mark is the socket mark of the proxy's own upstream connections. The firewall
// lets marked packets through untouched. Setting a mark needs CAP_NET_ADMIN,
// which the sandbox does not have.
const Mark uint32 = 0x4c5342 // "LSB"

const (
	tableName = "lsb"
	// natRangeProtoSpecified is NF_NAT_RANGE_PROTO_SPECIFIED: the redirect names a port.
	natRangeProtoSpecified = 2
	reg                    = 1
)

// InstallFirewall replaces the namespace's "lsb" tables with the rules that
// enforce the proxy:
//
//   - TCP that is not the proxy's own and not for loopback is redirected to proxyPort;
//   - UDP is dropped, except DNS to one of the dns servers;
//   - every other protocol, and all of IPv6 except loopback, is dropped.
//
// The rules live in the namespace, so they outlive the process that installed them.
func InstallFirewall(proxyPort uint16, dns []netip.Addr) error {
	c, err := nftables.New()
	if err != nil {
		return fmt.Errorf("netguard: open netlink: %w", err)
	}

	v4 := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: tableName})
	c.FlushTable(v4)

	nat := c.AddChain(&nftables.Chain{
		Name: "redirect", Table: v4, Type: nftables.ChainTypeNAT,
		Hooknum: nftables.ChainHookOutput, Priority: nftables.ChainPriorityNATDest,
	})
	c.AddRule(&nftables.Rule{Table: v4, Chain: nat, Exprs: join(matchMark(), accept())})
	c.AddRule(&nftables.Rule{Table: v4, Chain: nat, Exprs: join(matchLoopbackV4(), accept())})
	c.AddRule(&nftables.Rule{Table: v4, Chain: nat, Exprs: join(matchProto(unix.IPPROTO_TCP), []expr.Any{
		&expr.Immediate{Register: reg, Data: binaryutil.BigEndian.PutUint16(proxyPort)},
		&expr.Redir{RegisterProtoMin: reg, Flags: natRangeProtoSpecified},
	})})

	drop := nftables.ChainPolicyDrop
	filter := c.AddChain(&nftables.Chain{
		Name: "filter", Table: v4, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookOutput, Priority: nftables.ChainPriorityFilter, Policy: &drop,
	})
	c.AddRule(&nftables.Rule{Table: v4, Chain: filter, Exprs: join(matchMark(), accept())})
	c.AddRule(&nftables.Rule{Table: v4, Chain: filter, Exprs: join(matchLoopbackV4(), accept())})
	// The proxy's replies to the sandbox go to the address of the shared interface.
	c.AddRule(&nftables.Rule{Table: v4, Chain: filter, Exprs: join(matchEstablished(), accept())})
	for _, server := range dns {
		if !server.Is4() {
			continue
		}
		c.AddRule(&nftables.Rule{Table: v4, Chain: filter, Exprs: join(
			matchProto(unix.IPPROTO_UDP), matchDstPort(53), matchDstV4(server), accept())})
	}

	v6 := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv6, Name: tableName})
	c.FlushTable(v6)
	filter6 := c.AddChain(&nftables.Chain{
		Name: "filter", Table: v6, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookOutput, Priority: nftables.ChainPriorityFilter, Policy: &drop,
	})
	loopback6 := netip.IPv6Loopback().As16()
	c.AddRule(&nftables.Rule{Table: v6, Chain: filter6, Exprs: join([]expr.Any{
		&expr.Payload{DestRegister: reg, Base: expr.PayloadBaseNetworkHeader, Offset: 24, Len: 16},
		&expr.Cmp{Op: expr.CmpOpEq, Register: reg, Data: loopback6[:]},
	}, accept())})

	if err := c.Flush(); err != nil {
		return fmt.Errorf("netguard: install firewall: %w", err)
	}
	return nil
}

func join(parts ...[]expr.Any) []expr.Any {
	var out []expr.Any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func accept() []expr.Any { return []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}} }

func matchMark() []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyMARK, Register: reg},
		&expr.Cmp{Op: expr.CmpOpEq, Register: reg, Data: binaryutil.NativeEndian.PutUint32(Mark)},
	}
}

func matchProto(proto byte) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: reg},
		&expr.Cmp{Op: expr.CmpOpEq, Register: reg, Data: []byte{proto}},
	}
}

func matchLoopbackV4() []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: reg, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Bitwise{SourceRegister: reg, DestRegister: reg, Len: 4, Mask: []byte{0xff, 0, 0, 0}, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpEq, Register: reg, Data: []byte{127, 0, 0, 0}},
	}
}

func matchDstV4(addr netip.Addr) []expr.Any {
	b := addr.As4()
	return []expr.Any{
		&expr.Payload{DestRegister: reg, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: reg, Data: b[:]},
	}
}

func matchDstPort(port uint16) []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: reg, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: reg, Data: binaryutil.BigEndian.PutUint16(port)},
	}
}

func matchEstablished() []expr.Any {
	states := binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED)
	return []expr.Any{
		&expr.Ct{Register: reg, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: reg, DestRegister: reg, Len: 4, Mask: states, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: reg, Data: []byte{0, 0, 0, 0}},
	}
}
