package main

import "testing"

func TestCleanToken(t *testing.T) {
	want := "AbC123_xyz-987"
	for _, in := range []string{
		want, " " + want + "\n", "Bearer " + want, "bearer  " + want, `"` + want + `"`,
		`Authorization: Bearer ` + want, `curl "https://api.cloudflare.com/client/v4/user/tokens/verify" -H "Authorization: Bearer ` + want + `"`,
	} {
		if got := cleanToken(in); got != want {
			t.Errorf("cleanToken(%q) = %q", in, got)
		}
	}
	if cleanToken(keepToken) != keepToken || cleanToken("  ") != "" {
		t.Error("keep/empty")
	}
}
