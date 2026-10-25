package main

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"testing"
)

// TestDebugCFBSymmetry determines whether Go's NewCFBEncrypter is self-inverse,
// which OpenSSL's AES-CFB (used by shadowsocks-libev) is.
func TestDebugCFBSymmetry(t *testing.T) {
	key := make([]byte, 16)
	iv := make([]byte, 16)
	for i := range key {
		key[i] = byte(i)
	}
	for i := range iv {
		iv[i] = byte(i + 1)
	}
	block, _ := aes.NewCipher(key)
	payload := []byte("stream cipher payload!!") // 22 bytes, crosses the block boundary

	enc := cipher.NewCFBEncrypter(block, iv)
	ct := make([]byte, len(payload))
	enc.XORKeyStream(ct, payload)
	t.Logf("ciphertext        = %s", hex.EncodeToString(ct))

	selfInv := cipher.NewCFBEncrypter(block, iv)
	back1 := make([]byte, len(ct))
	selfInv.XORKeyStream(back1, ct)
	t.Logf("encrypter roundtrip = %q", back1)

	dec := cipher.NewCFBDecrypter(block, iv)
	back2 := make([]byte, len(ct))
	dec.XORKeyStream(back2, ct)
	t.Logf("decrypter roundtrip = %q", back2)
}
