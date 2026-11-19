package main

import "testing"

func TestParseNDT7(t *testing.T) {
	out := []byte(`{"Key":"measurement","Value":{"AppInfo":{"NumBytes":1}}}
{"ServerFQDN":"ndt-mlab1-den02.mlab-oti.measurement-lab.org","ServerIP":"1.2.3.4","Download":{"Value":512.25,"Unit":"Mbit/s"},"Upload":{"Value":41.5,"Unit":"Mbit/s"},"DownloadRetrans":{"Value":0.3,"Unit":"%"},"MinRTT":{"Value":11,"Unit":"ms"}}`)
	s, err := parseNDT7(out)
	if err != nil || s.Down != 512.25 || s.Up != 41.5 || s.Ping != 11 || s.Loss != 0.3 {
		t.Fatalf("parseNDT7 = %+v, %v", s, err)
	}
	if _, err := parseNDT7([]byte("not json")); err == nil {
		t.Fatal("want an error for output without a summary")
	}
}

func TestParseOokla(t *testing.T) {
	out := []byte(`{"type":"result","ping":{"jitter":0.437,"latency":36.504},"download":{"bandwidth":117938053,"latency":{"iqm":43.484}},"upload":{"bandwidth":5125601,"latency":{"iqm":32.715}},"packetLoss":0,"server":{"name":"Comcast","location":"Denver, CO"}}`)
	s, err := parseOokla(out)
	if err != nil || s.Down < 943.5 || s.Down > 943.6 || s.Up < 41.0 || s.Up > 41.01 || s.Ping != 36.504 || s.LoadedDn != 43.484 {
		t.Fatalf("parseOokla = %+v, %v", s, err)
	}
}

func TestStatOf(t *testing.T) {
	s := statOf([]float64{100, 90, 110, 0, 105, 95})
	if s.N != 5 || s.Median != 100 || s.Min != 90 || s.Max != 110 || s.SpreadPct != 10 {
		t.Fatalf("statOf = %+v", s)
	}
}
