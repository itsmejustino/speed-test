//go:build windows && (amd64 || arm64)

package main

import (
	"net"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

// GetAdaptersAddresses from iphlpapi.dll. The structures below follow
// IP_ADAPTER_ADDRESSES_LH and friends on 64-bit Windows, up to the last field
// read here; the rest of each record is left alone.

var procGetAdaptersAddresses = iphlpapi.NewProc("GetAdaptersAddresses")

type socketAddress struct {
	Sockaddr       *syscall.RawSockaddrAny
	SockaddrLength int32
}

type ipAdapterUnicastAddress struct {
	Length  uint32
	Flags   uint32
	Next    *ipAdapterUnicastAddress
	Address socketAddress
}

// The DNS server and gateway lists share this shape.
type ipAdapterAddr struct {
	Length   uint32
	Reserved uint32
	Next     *ipAdapterAddr
	Address  socketAddress
}

type ipAdapterAddresses struct {
	Length                 uint32
	IfIndex                uint32
	Next                   *ipAdapterAddresses
	AdapterName            *byte
	FirstUnicastAddress    *ipAdapterUnicastAddress
	FirstAnycastAddress    uintptr
	FirstMulticastAddress  uintptr
	FirstDNSServerAddress  *ipAdapterAddr
	DNSSuffix              *uint16
	Description            *uint16
	FriendlyName           *uint16
	PhysicalAddress        [8]byte
	PhysicalAddressLength  uint32
	Flags                  uint32
	MTU                    uint32
	IfType                 uint32
	OperStatus             uint32
	Ipv6IfIndex            uint32
	ZoneIndices            [16]uint32
	FirstPrefix            uintptr
	TransmitLinkSpeed      uint64
	ReceiveLinkSpeed       uint64
	FirstWinsServerAddress uintptr
	FirstGatewayAddress    *ipAdapterAddr
}

const (
	gaaFlagSkipAnycast     = 0x2
	gaaFlagSkipMulticast   = 0x4
	gaaFlagIncludeGateways = 0x80
	errorBufferOverflow    = 111
)

func utf16At(p *uint16) string {
	if p == nil {
		return ""
	}
	var s []uint16
	for ptr := unsafe.Pointer(p); ; ptr = unsafe.Add(ptr, 2) {
		c := *(*uint16)(ptr)
		if c == 0 {
			break
		}
		s = append(s, c)
		if len(s) > 1024 {
			break
		}
	}
	return syscall.UTF16ToString(s)
}

func sockaddrIP(sa socketAddress) net.IP {
	if sa.Sockaddr == nil {
		return nil
	}
	switch sa.Sockaddr.Addr.Family {
	case syscall.AF_INET:
		in := (*syscall.RawSockaddrInet4)(unsafe.Pointer(sa.Sockaddr))
		return net.IPv4(in.Addr[0], in.Addr[1], in.Addr[2], in.Addr[3])
	case syscall.AF_INET6:
		in := (*syscall.RawSockaddrInet6)(unsafe.Pointer(sa.Sockaddr))
		ip := make(net.IP, 16)
		copy(ip, in.Addr[:])
		return ip
	}
	return nil
}

func addrList(a *ipAdapterAddr) []string {
	var out []string
	for ; a != nil && len(out) < 8; a = a.Next {
		if ip := sockaddrIP(a.Address); ip != nil && !ip.IsLinkLocalUnicast() {
			out = append(out, ip.String())
		}
	}
	return out
}

func ifKind(t uint32, desc string) string {
	d := strings.ToLower(desc)
	switch {
	case t == 71:
		return "Wi-Fi"
	case t == 243 || t == 244:
		return "Cellular"
	case t == 131 || t == 23 || strings.Contains(d, "vpn") || strings.Contains(d, "tap-") || strings.Contains(d, "wireguard") || strings.Contains(d, "tunnel"):
		return "VPN or tunnel"
	case t == 6:
		return "Ethernet"
	}
	return "Other"
}

func linkMbps(bps uint64) float64 {
	if bps == 0 || bps == ^uint64(0) {
		return 0 // unknown
	}
	return float64(bps) / 1e6
}

func platformAdapter(local net.IP) *Adapter {
	if procGetAdaptersAddresses.Find() != nil {
		return nil
	}
	size := uint32(16 << 10)
	var buf []byte
	for i := 0; i < 4; i++ {
		buf = make([]byte, size)
		rc, _, _ := procGetAdaptersAddresses.Call(uintptr(syscall.AF_UNSPEC),
			gaaFlagIncludeGateways|gaaFlagSkipAnycast|gaaFlagSkipMulticast, 0,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
		if rc == 0 {
			break
		}
		if rc != errorBufferOverflow {
			return nil
		}
		buf = nil
	}
	if buf == nil {
		return nil
	}
	defer runtime.KeepAlive(buf)
	for aa := (*ipAdapterAddresses)(unsafe.Pointer(&buf[0])); aa != nil; aa = aa.Next {
		for u := aa.FirstUnicastAddress; u != nil; u = u.Next {
			if ip := sockaddrIP(u.Address); ip != nil && ip.Equal(local) {
				desc := utf16At(aa.Description)
				return &Adapter{
					Name:         utf16At(aa.FriendlyName),
					Description:  desc,
					Kind:         ifKind(aa.IfType, desc),
					LinkDownMbps: linkMbps(aa.ReceiveLinkSpeed),
					LinkUpMbps:   linkMbps(aa.TransmitLinkSpeed),
					MTU:          int(aa.MTU),
					Gateways:     addrList(aa.FirstGatewayAddress),
					DNS:          addrList(aa.FirstDNSServerAddress),
				}
			}
		}
	}
	return nil
}
