package main

import (
	"net"
	"testing"
)

// Describes the adapter that reaches the internet on whatever machine runs the
// tests, so CI logs show what each platform reports.
func TestCurrentAdapter(t *testing.T) {
	a := currentAdapter(net.ParseIP("1.1.1.1"))
	if a == nil {
		t.Skip("no route to the internet here")
	}
	t.Logf("%+v", *a)
	if a.LocalIP == "" || a.Name == "" {
		t.Fatalf("adapter missing name or local IP: %+v", *a)
	}
}
