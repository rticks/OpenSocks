package main

import "testing"

// ssBoot returns a minimal usable Shadowsocks protocol variant.
func ssBoot(method string) *bootsInfo {
	return &bootsInfo{Proto: "SS", Server: "5.6.7.8", Port: 443, Password: "pw", Method: method}
}

func trojanBoot() *bootsInfo {
	return &bootsInfo{Proto: "Trojan", Server: "tr.example.com", Port: 443, Password: "tpw"}
}

// TestSelectedSSBootPrefersDeclaredProtocol proves the engine follows the line's
// declared protocol when it is Shadowsocks, and skips Trojan variants.
func TestSelectedSSBootPrefersDeclaredProtocol(t *testing.T) {
	ss := ssBoot("chacha20-ietf-poly1305")
	resp := &connectResponse{
		Config: &connectConfig{
			Host:       "h.example.com",
			Proto:      5, // the line declares Trojan
			SSConf:     ss,
			TrojanConf: trojanBoot(),
		},
	}
	// A Trojan-declared line that also publishes SS must still connect over SS.
	got := selectedSSBoot(resp)
	if got == nil {
		t.Fatal("an SS configuration exists but was not selected")
	}
	if got.Proto != "SS" {
		t.Fatalf("selected %s instead of SS", got.Proto)
	}
	if got.Server != "5.6.7.8" {
		t.Fatalf("selected server %s", got.Server)
	}
}

// TestSelectedSSBootRejectsTrojanOnly proves a Trojan-only line is reported as
// unusable rather than being silently mis-configured.
func TestSelectedSSBootRejectsTrojanOnly(t *testing.T) {
	resp := &connectResponse{
		Config: &connectConfig{Host: "h.example.com", Proto: 5, TrojanConf: trojanBoot()},
	}
	if got := selectedSSBoot(resp); got != nil {
		t.Fatalf("selected %s for a Trojan-only line", got.Proto)
	}
	if _, err := upstreamFromBoot(selectedSSBoot(resp)); err == nil {
		t.Fatal("a Trojan-only line should produce an error")
	}
}

// TestSelectedSSBootSkipsIncompleteVariants proves a variant with a missing
// server or port is skipped in favour of a complete one.
func TestSelectedSSBootSkipsIncompleteVariants(t *testing.T) {
	resp := &connectResponse{
		Config: &connectConfig{
			Proto:  0,
			SSConf: &bootsInfo{Proto: "SS", Server: "", Port: 443, Password: "pw"}, // no server
			GTSConf: &bootsInfo{Proto: "GTS", Server: "9.9.9.9", Port: 0, Password: "pw"},
			SSWConf: ssBoot("aes-256-gcm"),
		},
	}
	got := selectedSSBoot(resp)
	if got == nil {
		t.Fatal("no variant selected")
	}
	if got.Server != "5.6.7.8" || got.Method != "aes-256-gcm" {
		t.Fatalf("selected %+v", got)
	}
}

// TestSelectedSSBootSearchesEveryConfig proves the fallback walks the Configs
// array when the primary Config carries nothing usable.
func TestSelectedSSBootSearchesEveryConfig(t *testing.T) {
	resp := &connectResponse{
		Config:  &connectConfig{Host: "h.example.com", Proto: 5, TrojanConf: trojanBoot()},
		Configs: []*connectConfig{{Proto: 0, SSConf: ssBoot("aes-128-gcm")}},
	}
	got := selectedSSBoot(resp)
	if got == nil || got.Method != "aes-128-gcm" {
		t.Fatalf("selected %+v", got)
	}
}

// TestSelectedSSBootNilResponse proves a nil response is handled.
func TestSelectedSSBootNilResponse(t *testing.T) {
	if got := selectedSSBoot(nil); got != nil {
		t.Fatalf("nil response produced %+v", got)
	}
}

// TestOrderedBootsPutsDeclaredFirst proves the protocol ordering helper keeps the
// declared protocol at the front and de-duplicates variants.
func TestOrderedBootsPutsDeclaredFirst(t *testing.T) {
	ss := ssBoot("aes-256-gcm")
	gts := &bootsInfo{Proto: "GTS", Server: "1.1.1.1", Port: 443, Password: "p"}
	ssw := &bootsInfo{Proto: "SSW", Server: "2.2.2.2", Port: 443, Password: "p"}
	tro := trojanBoot()

	cases := []struct {
		proto int
		want  string
	}{
		{0, "SS"},
		{1, "GTS"},
		{3, "SSW"},
		{5, "Trojan"},
	}
	for _, tc := range cases {
		out := orderedBoots(&connectConfig{Proto: tc.proto, SSConf: ss, GTSConf: gts, SSWConf: ssw, TrojanConf: tro})
		if len(out) != 4 {
			t.Fatalf("proto %d produced %d variants", tc.proto, len(out))
		}
		if out[0].Proto != tc.want {
			t.Fatalf("proto %d put %s first, want %s", tc.proto, out[0].Proto, tc.want)
		}
	}
	// A nil config must not panic and must yield no variants.
	if got := orderedBoots(nil); got != nil {
		t.Fatalf("nil config produced %d variants", len(got))
	}
}

// TestSSUpstreamValidation proves the upstream validator rejects each broken
// field independently.
func TestSSUpstreamValidation(t *testing.T) {
	good := ssUpstream{Server: "1.2.3.4", Port: 443, Password: "pw", Method: "aes-256-gcm"}
	if !good.valid() {
		t.Fatal("a complete upstream was rejected")
	}
	if good.address() != "1.2.3.4:443" {
		t.Fatalf("address = %s", good.address())
	}
	broken := []ssUpstream{
		{Server: "", Port: 443, Password: "pw"},
		{Server: "1.2.3.4", Port: 0, Password: "pw"},
		{Server: "1.2.3.4", Port: 70000, Password: "pw"},
		{Server: "1.2.3.4", Port: 443, Password: ""},
	}
	for _, u := range broken {
		if u.valid() {
			t.Fatalf("incomplete upstream %+v was accepted", u)
		}
	}
}

// TestUpstreamFromBootNormalisesFields proves whitespace is trimmed and a missing
// method falls back to the service default.
func TestUpstreamFromBootNormalisesFields(t *testing.T) {
	u, err := upstreamFromBoot(&bootsInfo{Proto: "SS", Server: "  1.2.3.4  ", Port: 8388, Password: "pw", Method: "  "})
	if err != nil {
		t.Fatal(err)
	}
	if u.Server != "1.2.3.4" {
		t.Fatalf("server = %q", u.Server)
	}
	if u.Method != defaultSSMethod {
		t.Fatalf("method = %q", u.Method)
	}
}

// TestSSUpstreamRejectsUnsupportedMethod proves an unknown cipher name is caught
// at connect time rather than on the first proxied request.
func TestSSUpstreamRejectsUnsupportedMethod(t *testing.T) {
	for _, method := range []string{"2022-blake3-aes-256-gcm", "rc4", "aes-256-ctr", "plain"} {
		if _, err := upstreamFromBoot(&bootsInfo{Proto: "SS", Server: "1.2.3.4", Port: 443, Password: "pw", Method: method}); err == nil {
			t.Fatalf("method %q was accepted", method)
		}
	}
}
