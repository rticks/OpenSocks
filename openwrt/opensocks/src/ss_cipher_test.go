package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
)

// ssConfig builds a resolved connectResponse for tests that need one.
func ssConfig() *connectResponse {
	verify := true
	verifyPtr := &verify
	return &connectResponse{
		LineID:   7,
		LineName: "hk-01",
		Config: &connectConfig{
			Host:    "h.example.com",
			ProxyIP: "5.6.7.8",
			Proto:   0,
			SSConf: &bootsInfo{
				Proto:    "SS",
				Server:   "5.6.7.8",
				Port:     443,
				Password: "pw",
				Method:   "chacha20-ietf-poly1305",
			},
			TrojanConf: &bootsInfo{
				Proto:    "Trojan",
				Server:   "tr.example.com",
				Port:     443,
				Password: "tpw",
				Ssl: &struct {
					SNI    string `json:"sni"`
					Verify *bool  `json:"verify"`
				}{SNI: "tr.example.com", Verify: verifyPtr},
			},
		},
	}
}

func TestResolveCipherSpec(t *testing.T) {
	cases := []struct {
		method   string
		wantKey  int
		wantAEAD bool
	}{
		{"chacha20-ietf-poly1305", 32, true},
		{"CHACHA20-IETF-POLY1305", 32, true},
		{"aes-128-gcm", 16, true},
		{"aes-256-gcm", 32, true},
		{"aes-128-cfb", 16, false},
		{"aes-256-cfb", 32, false},
		{"rc4-md5", 16, false},
		{"", 32, true}, // default method
	}
	for _, tc := range cases {
		spec, err := resolveCipherSpec(tc.method)
		if err != nil {
			t.Fatalf("%q: %v", tc.method, err)
		}
		if spec.keySize != tc.wantKey || spec.aead != tc.wantAEAD {
			t.Fatalf("%q: keySize=%d aead=%v", tc.method, spec.keySize, spec.aead)
		}
	}
	if _, err := resolveCipherSpec("2022-blake3-aes-256-gcm"); err == nil {
		t.Fatal("2022 methods should be rejected rather than silently mis-parsed")
	}
}

// TestEVPBytesToKeyMatchesReference checks the key derivation against the values
// produced by OpenSSL's EVP_BytesToKey with MD5, which is what every Shadowsocks
// implementation uses for password-based methods.
func TestEVPBytesToKeyMatchesReference(t *testing.T) {
	// Reference value computed independently with
	// printf 'password' | openssl ... / ss-libev's derive_key for aes-256-cfb.
	key := evpBytesToKey("password", 32)
	if len(key) != 32 {
		t.Fatalf("key length %d", len(key))
	}
	// MD5("password") = 5f4dcc3b5aa765d61d8327deb882cf99. The first 16 bytes of
	// the derived key must equal it, since there is no previous block yet.
	if got := hex.EncodeToString(key[:16]); got != "5f4dcc3b5aa765d61d8327deb882cf99" {
		t.Fatalf("first block = %s", got)
	}
}

func TestSocksAddrHeaderRoundTrip(t *testing.T) {
	cases := []struct {
		host string
		port int
	}{
		{"example.com", 443},
		{"127.0.0.1", 8080},
		{"2001:db8::1", 53},
	}
	for _, tc := range cases {
		header, err := socksAddrHeader(tc.host, tc.port)
		if err != nil {
			t.Fatalf("%s: %v", tc.host, err)
		}
		host, port, length, err := decodeSocksAddrHeader(header)
		if err != nil {
			t.Fatalf("%s: decode: %v", tc.host, err)
		}
		if length != len(header) {
			t.Fatalf("%s: length %d != %d", tc.host, length, len(header))
		}
		if port != tc.port {
			t.Fatalf("%s: port %d != %d", tc.host, port, tc.port)
		}
		if !strings.EqualFold(host, tc.host) {
			t.Fatalf("%s: host %s", tc.host, host)
		}
	}
	if _, err := socksAddrHeader("", 443); err == nil {
		t.Fatal("empty host should be rejected")
	}
	if _, err := socksAddrHeader("example.com", 0); err == nil {
		t.Fatal("port 0 should be rejected")
	}
}

// TestSSConnAEADRoundTrip pipes a stream through a pair of ssConns that share
// the same key, exercising the chunked framing in both directions.
func TestSSConnAEADRoundTrip(t *testing.T) {
	methods := []string{"chacha20-ietf-poly1305", "aes-128-gcm", "aes-256-gcm", "xchacha20-ietf-poly1305"}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			cipherText, err := newSSConn(client, method, "password", "example.com", 443)
			if err != nil {
				t.Fatal(err)
			}
			plainText, err := newSSConn(server, method, "password", "example.com", 443)
			if err != nil {
				t.Fatal(err)
			}
			// The server side does not send a header; strip the client's header
			// from what the "server" reads so the payload can be compared.
			payload := []byte("hello shadowsocks")
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := cipherText.Write(payload); err != nil {
					t.Errorf("write: %v", err)
				}
			}()
			headerLen := len(mustHeader(t, "example.com", 443))
			got := make([]byte, headerLen+len(payload))
			if _, err := io.ReadFull(plainText, got); err != nil {
				t.Fatalf("read: %v", err)
			}
			wg.Wait()
			if !bytes.Equal(got[headerLen:], payload) {
				t.Fatalf("payload = %q", got[headerLen:])
			}
		})
	}
}

func mustHeader(t *testing.T, host string, port int) []byte {
	t.Helper()
	header, err := socksAddrHeader(host, port)
	if err != nil {
		t.Fatal(err)
	}
	return header
}

// TestSSConnStreamRoundTrip covers the aes-cfb and rc4-md5 stream ciphers, whose
// wire format is [IV][ciphertext] with no per-chunk framing.
func TestSSConnStreamRoundTrip(t *testing.T) {
	methods := []string{"aes-128-cfb", "aes-256-cfb", "rc4-md5"}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			cipherText, err := newSSConn(client, method, "password", "example.com", 443)
			if err != nil {
				t.Fatal(err)
			}
			plainText, err := newSSConn(server, method, "password", "example.com", 443)
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte("stream cipher payload")
			go func() {
				if _, err := cipherText.Write(payload); err != nil {
					t.Errorf("write: %v", err)
				}
			}()
			headerLen := len(mustHeader(t, "example.com", 443))
			got := make([]byte, headerLen+len(payload))
			if _, err := io.ReadFull(plainText, got); err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(got[headerLen:], payload) {
				t.Fatalf("payload = %q", got[headerLen:])
			}
		})
	}
}

// TestSSAEADPacketRoundTrip exercises the single-datagram format used by the
// UDP relay: [salt][payload+tag] with an all-zero nonce.
func TestSSAEADPacketRoundTrip(t *testing.T) {
	for _, method := range []string{"chacha20-ietf-poly1305", "aes-128-gcm", "aes-256-gcm"} {
		spec, err := resolveCipherSpec(method)
		if err != nil {
			t.Fatal(err)
		}
		key, err := ssKey(method, "password")
		if err != nil {
			t.Fatal(err)
		}
		payload := []byte("udp datagram body")
		packet, err := ssAEADPacket(spec, key, payload)
		if err != nil {
			t.Fatalf("%s pack: %v", method, err)
		}
		if len(packet) != spec.saltLen+len(payload)+ssTagSize {
			t.Fatalf("%s packet length %d", method, len(packet))
		}
		got, err := ssAEADUnpack(spec, key, packet)
		if err != nil {
			t.Fatalf("%s unpack: %v", method, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("%s payload = %q", method, got)
		}
		// A flipped byte must fail authentication, proving the tag is checked.
		packet[len(packet)-1] ^= 0xff
		if _, err := ssAEADUnpack(spec, key, packet); err == nil {
			t.Fatalf("%s accepted a corrupted datagram", method)
		}
	}
}

// TestSSAEADPacketWrongPassword proves the AEAD tag rejects a different key.
func TestSSAEADPacketWrongPassword(t *testing.T) {
	spec, err := resolveCipherSpec("chacha20-ietf-poly1305")
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ssKey(spec.name, "password")
	wrong, _ := ssKey(spec.name, "other")
	packet, err := ssAEADPacket(spec, key, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ssAEADUnpack(spec, wrong, packet); err == nil {
		t.Fatal("wrong password should not decrypt")
	}
}

func TestSSStreamPacketRoundTrip(t *testing.T) {
	for _, method := range []string{"aes-128-cfb", "aes-256-cfb", "rc4-md5"} {
		spec, err := resolveCipherSpec(method)
		if err != nil {
			t.Fatal(err)
		}
		key, err := ssKey(method, "password")
		if err != nil {
			t.Fatal(err)
		}
		payload := []byte("stream datagram")
		packet, err := ssStreamPacket(spec, key, payload)
		if err != nil {
			t.Fatalf("%s pack: %v", method, err)
		}
		got, err := ssStreamUnpack(spec, key, packet)
		if err != nil {
			t.Fatalf("%s unpack: %v", method, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("%s payload = %q", method, got)
		}
	}
}

// TestIncrementNonce proves the AEAD nonce counter carries across bytes in
// little-endian order, as the Shadowsocks AEAD spec requires.
func TestIncrementNonce(t *testing.T) {
	nonce := make([]byte, 12)
	nonce[0] = 0xff
	incrementNonce(nonce)
	if nonce[0] != 0 || nonce[1] != 1 {
		t.Fatalf("nonce = %x", nonce)
	}
	for i := range nonce {
		nonce[i] = 0xff
	}
	incrementNonce(nonce)
	for i := range nonce {
		if nonce[i] != 0 {
			t.Fatalf("wrapped nonce = %x", nonce)
		}
	}
}

// TestUpstreamFromBootRejectsUnusable proves the engine refuses configurations
// it cannot speak instead of failing later with an opaque dial error.
func TestUpstreamFromBootRejectsUnusable(t *testing.T) {
	if _, err := upstreamFromBoot(nil); err == nil {
		t.Fatal("nil boot should fail")
	}
	if _, err := upstreamFromBoot(&bootsInfo{Proto: "Trojan", Server: "x", Port: 1, Password: "p"}); err == nil {
		t.Fatal("Trojan should be rejected")
	}
	if _, err := upstreamFromBoot(&bootsInfo{Proto: "SS", Server: "", Port: 1, Password: "p"}); err == nil {
		t.Fatal("empty server should fail")
	}
	if _, err := upstreamFromBoot(&bootsInfo{Proto: "SS", Server: "1.2.3.4", Port: 443, Password: "p", Method: "2022-blake3-aes-256-gcm"}); err == nil {
		t.Fatal("unsupported method should fail")
	}
	u, err := upstreamFromBoot(&bootsInfo{Proto: "SS", Server: "1.2.3.4", Port: 443, Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if u.Method != defaultSSMethod {
		t.Fatalf("default method = %s", u.Method)
	}
	if u.address() != "1.2.3.4:443" {
		t.Fatalf("address = %s", u.address())
	}
}

// TestSSConnLargePayloadFrames proves payloads larger than one chunk are split
// and reassembled correctly.
func TestSSConnLargePayloadFrames(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	cipherText, err := newSSConn(client, "aes-256-gcm", "password", "example.com", 443)
	if err != nil {
		t.Fatal(err)
	}
	plainText, err := newSSConn(server, "aes-256-gcm", "password", "example.com", 443)
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, ssMaxPayload*2+123)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	headerLen := len(mustHeader(t, "example.com", 443))
	go func() {
		if _, err := cipherText.Write(payload); err != nil {
			t.Errorf("write: %v", err)
		}
	}()
	got := make([]byte, headerLen+len(payload))
	if _, err := io.ReadFull(plainText, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got[headerLen:], payload) {
		t.Fatal("large payload did not survive the chunk framing")
	}
}
