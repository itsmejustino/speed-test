//go:build linux

package main

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"net"
	"os"
	"strconv"
	"strings"
)

// On Linux, /sys/class/net has the link speed and type, /proc/net/route the
// default gateway, and /etc/resolv.conf the DNS servers.
func platformAdapter(local net.IP) *Adapter {
	ifc, ok := interfaceFor(local)
	if !ok {
		return nil
	}
	a := &Adapter{Name: ifc.Name, MTU: ifc.MTU, Kind: "Other"}
	sys := "/sys/class/net/" + ifc.Name
	_, wireless := os.Stat(sys + "/wireless")
	typ, _ := os.ReadFile(sys + "/type")
	switch {
	case wireless == nil:
		a.Kind = "Wi-Fi"
	case strings.HasPrefix(ifc.Name, "tun") || strings.HasPrefix(ifc.Name, "wg") || strings.HasPrefix(ifc.Name, "tap"):
		a.Kind = "VPN or tunnel"
	case strings.TrimSpace(string(typ)) == "1":
		a.Kind = "Ethernet"
	}
	if b, err := os.ReadFile(sys + "/speed"); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && v > 0 {
			a.LinkDownMbps, a.LinkUpMbps = float64(v), float64(v)
		}
	}
	a.Gateways = linuxGateways(ifc.Name)
	a.DNS = resolvConf()
	return a
}

func linuxGateways(name string) []string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fl := strings.Fields(sc.Text())
		if len(fl) < 3 || fl[0] != name || fl[1] != "00000000" {
			continue
		}
		b, err := hex.DecodeString(fl[2])
		if err != nil || len(b) != 4 {
			continue
		}
		v := binary.LittleEndian.Uint32(b)
		out = append(out, net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).String())
	}
	return out
}

func resolvConf() []string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		fl := strings.Fields(line)
		if len(fl) >= 2 && fl[0] == "nameserver" {
			out = append(out, fl[1])
		}
	}
	return out
}
