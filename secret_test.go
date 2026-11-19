package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The token goes to disk encrypted where Windows can encrypt it, never as
// text, and comes back unchanged.
func TestTokenAtRest(t *testing.T) {
	old := baseDir
	baseDir = t.TempDir()
	defer func() { baseDir = old }()

	const tok = "AbC123_xyz-987"
	s := defaultSettings()
	s.RadarToken = tok
	if err := saveSettings(s); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(baseDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if secretSupported() {
		if strings.Contains(string(b), tok) || !strings.Contains(string(b), secretPrefix) {
			t.Fatalf("token not encrypted on disk:\n%s", b)
		}
	}
	if got := loadSettings().RadarToken; got != tok {
		t.Fatalf("token after load = %q", got)
	}
	if fi, err := os.Stat(filepath.Join(baseDir, "settings.json")); err == nil && !secretSupported() && fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("settings.json mode %v, want readable by this user only", fi.Mode().Perm())
	}
}

// A token saved as text by an older version is encrypted on the next load.
func TestTokenMigrates(t *testing.T) {
	if !secretSupported() {
		t.Skip("no DPAPI here")
	}
	old := baseDir
	baseDir = t.TempDir()
	defer func() { baseDir = old }()
	p := filepath.Join(baseDir, "settings.json")
	if err := os.WriteFile(p, []byte(`{"radarToken":"PlainOld_123"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if loadSettings().RadarToken != "PlainOld_123" {
		t.Fatal("old token lost")
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "PlainOld_123") {
		t.Fatalf("old token still in the clear:\n%s", b)
	}
}

// A token encrypted for another account or computer cannot be read: it is
// dropped with a note rather than sent to Cloudflare as garbage.
func TestTokenUnreadable(t *testing.T) {
	old := baseDir
	baseDir = t.TempDir()
	defer func() { baseDir = old }()
	p := filepath.Join(baseDir, "settings.json")
	if err := os.WriteFile(p, []byte(`{"radarToken":"dpapi:bm90IGEgcmVhbCBibG9i"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadSettings().RadarToken; got != "" {
		t.Fatalf("unreadable token kept as %q", got)
	}
}
