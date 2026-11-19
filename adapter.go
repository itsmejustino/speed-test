package main

import (
	"net"
	"time"
)

// Adapter is the network interface this computer uses to reach the server: the
// first thing to rule in or out when a line tests slow.
type Adapter struct {
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	Kind         string   `json:"kind"`                   // Ethernet, Wi-Fi, Cellular, VPN or tunnel, Other
	LinkDownMbps float64  `json:"linkDownMbps,omitempty"` // negotiated link (PHY) rate, receive
	LinkUpMbps   float64  `json:"linkUpMbps,omitempty"`   // negotiated link (PHY) rate, transmit
	MTU          int      `json:"mtu,omitempty"`
	LocalIP      string   `json:"localIp"`
	Gateways     []string `json:"gateways,omitempty"`
	DNS          []string `json:"dns,omitempty"`
}

// Setup is how long the first connection to the server took to open.
type Setup struct {
	DNSMs      float64 `json:"dnsMs"`
	TCPMs      float64 `json:"tcpMs"`
	TLSMs      float64 `json:"tlsMs"`
	TTFBMs     float64 `json:"ttfbMs"` // request sent to first byte of the answer, including the server's work
	Protocol   string  `json:"protocol"`
	TLSVersion string  `json:"tlsVersion,omitempty"`
}

func ms(d time.Duration) float64 { return round3(float64(d.Microseconds()) / 1000) }

// localIPFor asks the system which local address it would use to reach ip. A
// UDP "connection" sends nothing; it only picks the route.
func localIPFor(ip net.IP) net.IP {
	c, err := net.Dial("udp", net.JoinHostPort(ip.String(), "443"))
	if err != nil {
		return nil
	}
	defer func() { _ = c.Close() }()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return a.IP
	}
	return nil
}

// interfaceFor finds the interface that holds ip, with what Go knows about it
// on every system: name and MTU.
func interfaceFor(ip net.IP) (*net.Interface, bool) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, false
	}
	for i := range ifs {
		addrs, err := ifs[i].Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
				return &ifs[i], true
			}
		}
	}
	return nil, false
}

// currentAdapter describes the adapter used to reach server, or nil.
func currentAdapter(server net.IP) *Adapter {
	local := localIPFor(server)
	if local == nil {
		return nil
	}
	a := platformAdapter(local)
	if a == nil {
		a = &Adapter{Kind: "Other"}
		if ifc, ok := interfaceFor(local); ok {
			a.Name, a.MTU = ifc.Name, ifc.MTU
		}
	}
	a.LocalIP = local.String()
	return a
}
