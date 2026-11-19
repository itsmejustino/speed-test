//go:build !windows

package main

import (
	"errors"
	"net"
	"time"
)

// Outside Windows, ICMP needs raw sockets (and usually root), so the program
// measures latency with TCP connections instead.
func icmpSupported() bool { return false }

func icmpPing(ip net.IP, timeout time.Duration) (time.Duration, error) {
	return 0, errors.New("ICMP not supported here")
}

func icmpProbeTTL(ip net.IP, ttl int, timeout time.Duration) (net.IP, time.Duration, bool, error) {
	return nil, 0, false, errors.New("ICMP not supported here")
}
