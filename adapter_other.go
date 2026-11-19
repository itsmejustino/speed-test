//go:build !linux && !(windows && (amd64 || arm64))

package main

import "net"

// Elsewhere only the interface name and MTU are known (see currentAdapter).
func platformAdapter(net.IP) *Adapter { return nil }
