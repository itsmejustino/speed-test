//go:build !windows

package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

type tcpCounters struct{ sent, retrans int64 }

// Linux: the Tcp line of /proc/net/snmp.
func readTCPCounters() (tcpCounters, error) {
	b, err := os.ReadFile("/proc/net/snmp")
	if err != nil {
		return tcpCounters{}, err
	}
	var head, vals []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "Tcp:") {
			if head == nil {
				head = strings.Fields(line)
			} else {
				vals = strings.Fields(line)
			}
		}
	}
	var c tcpCounters
	found := 0
	for i := range head {
		if i >= len(vals) {
			break
		}
		v, _ := strconv.ParseInt(vals[i], 10, 64)
		switch head[i] {
		case "OutSegs":
			c.sent, found = v, found+1
		case "RetransSegs":
			c.retrans, found = v, found+1
		}
	}
	if found < 2 {
		return c, errors.New("counters not found")
	}
	return c, nil
}
