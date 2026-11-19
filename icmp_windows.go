//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"net"
	"syscall"
	"time"
	"unsafe"
)

// ICMP echo through Windows' own ping API (iphlpapi.dll). Unlike raw sockets,
// it needs no administrator rights.

var (
	iphlpapi        = syscall.NewLazyDLL("iphlpapi.dll")
	procCreateFile  = iphlpapi.NewProc("IcmpCreateFile")
	procSendEcho    = iphlpapi.NewProc("IcmpSendEcho")
	procCloseHandle = iphlpapi.NewProc("IcmpCloseHandle")
)

func icmpSupported() bool { return procSendEcho.Find() == nil }

func icmpPing(ip net.IP, timeout time.Duration) (time.Duration, error) {
	ip4 := ip.To4()
	if ip4 == nil {
		return 0, errors.New("not IPv4")
	}
	h, _, _ := procCreateFile.Call()
	if h == 0 || h == uintptr(syscall.InvalidHandle) {
		return 0, errors.New("IcmpCreateFile failed")
	}
	defer func() { _, _, _ = procCloseHandle.Call(h) }()

	// IPAddr is the address in network byte order, as it sits in memory.
	addr := binary.LittleEndian.Uint32(ip4)
	data := make([]byte, 32)
	for i := range data {
		data[i] = byte('a' + i%23)
	}
	// ICMP_ECHO_REPLY (40 bytes on 64-bit) + echoed data + room for an ICMP error.
	reply := make([]byte, 64+len(data)+64)

	start := time.Now()
	n, _, _ := procSendEcho.Call(h, uintptr(addr),
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), 0,
		uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), uintptr(timeout.Milliseconds()))
	elapsed := time.Since(start)
	if n == 0 {
		return 0, errors.New("no reply")
	}
	if status := binary.LittleEndian.Uint32(reply[4:8]); status != 0 {
		return 0, errors.New("ICMP error")
	}
	// The API reports whole milliseconds; the clock around the call is finer.
	// Fall back to the API's figure only if the clock somehow reads less.
	if api := time.Duration(binary.LittleEndian.Uint32(reply[8:12])) * time.Millisecond; elapsed < api {
		elapsed = api
	}
	return elapsed, nil
}

const (
	ipSuccess           = 0
	ipTTLExpiredTransit = 11013
	ipTTLExpiredReassem = 11014
)

// icmpProbeTTL sends one echo with the given time-to-live. It returns who
// answered: a router on the way (TTL expired) or the destination itself.
func icmpProbeTTL(ip net.IP, ttl int, timeout time.Duration) (net.IP, time.Duration, bool, error) {
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, 0, false, errors.New("not IPv4")
	}
	h, _, _ := procCreateFile.Call()
	if h == 0 || h == uintptr(syscall.InvalidHandle) {
		return nil, 0, false, errors.New("IcmpCreateFile failed")
	}
	defer func() { _, _, _ = procCloseHandle.Call(h) }()

	// IP_OPTION_INFORMATION on 64-bit Windows: Ttl, Tos, Flags, OptionsSize (one byte
	// each), four bytes of padding, then the OptionsData pointer.
	var opts [16]byte
	opts[0] = byte(ttl)
	data := make([]byte, 32)
	reply := make([]byte, 64+len(data)+64)
	addr := binary.LittleEndian.Uint32(ip4)

	start := time.Now()
	n, _, _ := procSendEcho.Call(h, uintptr(addr),
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)),
		uintptr(unsafe.Pointer(&opts[0])),
		uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), uintptr(timeout.Milliseconds()))
	elapsed := time.Since(start)
	// Windows can report a TTL-expired answer either as a reply or as a failure
	// with the reply filled in, so read the reply either way.
	from := net.IP(append([]byte(nil), reply[0:4]...))
	status := binary.LittleEndian.Uint32(reply[4:8])
	if from.Equal(net.IPv4zero) {
		if n == 0 {
			return nil, 0, false, errors.New("no reply")
		}
	}
	switch status {
	case ipSuccess:
		return from, elapsed, from.Equal(ip4), nil
	case ipTTLExpiredTransit, ipTTLExpiredReassem:
		return from, elapsed, false, nil
	}
	if !from.Equal(net.IPv4zero) && status >= 11002 && status <= 11005 {
		return from, elapsed, false, nil // unreachable, reported by a router
	}
	return nil, 0, false, errors.New("no reply")
}
