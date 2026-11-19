//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"syscall"
	"unsafe"
)

type tcpCounters struct{ sent, retrans int64 }

var procTCPStats = iphlpapi.NewProc("GetTcpStatisticsEx")

// Windows' TCP counters for the whole computer (IPv4 and IPv6), straight from
// the network stack. They include any other traffic on the computer during the
// test. The counters are 32-bit, so the difference is taken modulo 2^32.
func readTCPCounters() (tcpCounters, error) {
	if procTCPStats.Find() != nil {
		return tcpCounters{}, errors.New("GetTcpStatisticsEx not available")
	}
	var c tcpCounters
	ok := 0
	for _, fam := range []uintptr{syscall.AF_INET, 23 /* AF_INET6 */} {
		var buf [15 * 4]byte // MIB_TCPSTATS: fifteen DWORDs
		r, _, _ := procTCPStats.Call(uintptr(unsafe.Pointer(&buf[0])), fam)
		if r != 0 {
			continue
		}
		c.sent += int64(binary.LittleEndian.Uint32(buf[10*4:]))    // dwOutSegs
		c.retrans += int64(binary.LittleEndian.Uint32(buf[11*4:])) // dwRetransSegs
		ok++
	}
	if ok == 0 {
		return c, errors.New("GetTcpStatisticsEx failed")
	}
	return c, nil
}
