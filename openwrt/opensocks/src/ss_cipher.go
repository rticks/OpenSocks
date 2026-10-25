package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rc4"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// Shadowsocks wire protocol, implemented in-process so the daemon no longer
// needs an external ss-redir/ss-local helper. OpenWrt 25.12 dropped the
// shadowsocks-libev package set from its feed, and shipping a second binary
// would mean a per-architecture cross build for routers that cannot spare the
// space of a large proxy core.
//
// Two cipher families are supported:
//
//   - AEAD (aes-128/192/256-gcm, chacha20-ietf-poly1305,
//     xchacha20-ietf-poly1305). This is what current Transocks lines use.
//   - Stream (aes-*-cfb, rc4-md5), kept for servers that still advertise them.
//     A stream connection is unauthenticated by design, matching the original
//     Shadowsocks protocol.
//
// The 2022 "2022-blake3-*" methods are deliberately not implemented: they are
// replay-protected AEAD variants with a different key schedule and the service
// does not offer them.

const (
	ssMaxChunkSize = 0x3FFF
	ssMaxPayload   = 0x3FFF - 64
	ssTagSize      = 16
	ssIVSize       = 16
	ssKeyInfo      = "ss-subkey"
	ssMethodGCM128 = "aes-128-gcm"
	ssMethodGCM192 = "aes-192-gcm"
	ssMethodGCM256 = "aes-256-gcm"
	ssMethodChaCha = "chacha20-ietf-poly1305"
	ssMethodXCha   = "xchacha20-ietf-poly1305"
	ssMethodCFB128 = "aes-128-cfb"
	ssMethodCFB192 = "aes-192-cfb"
	ssMethodCFB256 = "aes-256-cfb"
	ssMethodRC4MD5 = "rc4-md5"
)

// defaultSSMethod matches the service default reported by ss-libev.
const defaultSSMethod = ssMethodChaCha

var errCipherNotSupported = errors.New("shadowsocks cipher not supported")

// ssCipherSpec describes how a named method turns a password into keys.
type ssCipherSpec struct {
	name    string
	keySize int
	saltLen int
	aead    bool
	// streamIVLen is the per-connection IV length for stream ciphers.
	streamIVLen int
}

// ssCiphers maps every accepted method name to its parameters. Salt length
// follows the AEAD specification: the key size when it exceeds 16, else 16.
var ssCiphers = map[string]ssCipherSpec{
	ssMethodGCM128: {name: ssMethodGCM128, keySize: 16, saltLen: 16, aead: true},
	ssMethodGCM192: {name: ssMethodGCM192, keySize: 24, saltLen: 24, aead: true},
	ssMethodGCM256: {name: ssMethodGCM256, keySize: 32, saltLen: 32, aead: true},
	ssMethodChaCha: {name: ssMethodChaCha, keySize: 32, saltLen: 32, aead: true},
	ssMethodXCha:   {name: ssMethodXCha, keySize: 32, saltLen: 32, aead: true},
	ssMethodCFB128: {name: ssMethodCFB128, keySize: 16, saltLen: ssIVSize, streamIVLen: ssIVSize},
	ssMethodCFB192: {name: ssMethodCFB192, keySize: 24, saltLen: ssIVSize, streamIVLen: ssIVSize},
	ssMethodCFB256: {name: ssMethodCFB256, keySize: 32, saltLen: ssIVSize, streamIVLen: ssIVSize},
	ssMethodRC4MD5: {name: ssMethodRC4MD5, keySize: 16, saltLen: ssIVSize, streamIVLen: ssIVSize},
}

// resolveCipherSpec normalises the method string reported by the service.
func resolveCipherSpec(method string) (ssCipherSpec, error) {
	name := strings.ToLower(strings.TrimSpace(method))
	if name == "" {
		name = defaultSSMethod
	}
	if spec, ok := ssCiphers[name]; ok {
		return spec, nil
	}
	return ssCipherSpec{}, fmt.Errorf("%w: %s", errCipherNotSupported, method)
}

// evpBytesToKey derives the master key from the password exactly like
// OpenSSL's EVP_BytesToKey with MD5 and no salt, which is what every
// Shadowsocks implementation uses.
func evpBytesToKey(password string, keyLen int) []byte {
	var out, prev []byte
	h := md5.New()
	for len(out) < keyLen {
		h.Reset()
		h.Write(prev)
		h.Write([]byte(password))
		prev = h.Sum(nil)
		out = append(out, prev...)
	}
	return out[:keyLen]
}

// ssKey derives the master key for a method. The service always sends a
// password; base64 pre-shared keys are not used by Transocks lines.
func ssKey(method, password string) ([]byte, error) {
	spec, err := resolveCipherSpec(method)
	if err != nil {
		return nil, err
	}
	return evpBytesToKey(password, spec.keySize), nil
}

// ssAEAD derives the per-salt subkey and constructs the AEAD instance.
func ssAEAD(spec ssCipherSpec, key, salt []byte) (cipher.AEAD, error) {
	subkey := make([]byte, spec.keySize)
	r := hkdf.New(sha1.New, key, salt, []byte(ssKeyInfo))
	if _, err := io.ReadFull(r, subkey); err != nil {
		return nil, err
	}
	switch spec.name {
	case ssMethodGCM128, ssMethodGCM192, ssMethodGCM256:
		block, err := aes.NewCipher(subkey)
		if err != nil {
			return nil, err
		}
		return cipher.NewGCM(block)
	case ssMethodChaCha:
		return chacha20poly1305.New(subkey)
	case ssMethodXCha:
		return chacha20poly1305.NewX(subkey)
	}
	return nil, fmt.Errorf("%w: %s", errCipherNotSupported, spec.name)
}

// ssStream returns a stream cipher for the stream methods. aes-*-cfb uses the
// key directly; rc4-md5 derives a per-connection RC4 key from key||IV.
//
// encrypt selects the CFB direction. OpenSSL's AES-CFB (what shadowsocks-libev
// uses) is self-inverse, but Go's cipher.NewCFBEncrypter is not: it reuses the
// CFB state without the final partial-block feedback, so decrypting with the
// "encrypter" corrupts everything past the first block. Callers must pick the
// matching direction for each side of the connection. Both directions copy the
// IV first, because Go's CFB implementation keeps updating the caller's slice.
func ssStream(spec ssCipherSpec, key, iv []byte, encrypt bool) (cipher.Stream, error) {
	switch spec.name {
	case ssMethodCFB128, ssMethodCFB192, ssMethodCFB256:
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		if len(iv) != block.BlockSize() {
			return nil, fmt.Errorf("aes-cfb IV length %d is invalid", len(iv))
		}
		state := make([]byte, len(iv))
		copy(state, iv)
		if encrypt {
			return cipher.NewCFBEncrypter(block, state), nil
		}
		return cipher.NewCFBDecrypter(block, state), nil
	case ssMethodRC4MD5:
		sum := md5.New()
		sum.Write(key)
		sum.Write(iv)
		return rc4.NewCipher(sum.Sum(nil))
	}
	return nil, fmt.Errorf("%w: %s", errCipherNotSupported, spec.name)
}

// ssAEADPacket encrypts one datagram: [salt][payload+tag] with an all-zero
// nonce, as required by the Shadowsocks AEAD UDP specification.
func ssAEADPacket(spec ssCipherSpec, key, payload []byte) ([]byte, error) {
	salt := make([]byte, spec.saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	aead, err := ssAEAD(spec, key, salt)
	if err != nil {
		return nil, err
	}
	out := make([]byte, spec.saltLen, spec.saltLen+len(payload)+ssTagSize)
	copy(out, salt)
	return aead.Seal(out, make([]byte, aead.NonceSize()), payload, nil), nil
}

// ssAEADUnpack decrypts one datagram produced by ssAEADPacket.
func ssAEADUnpack(spec ssCipherSpec, key, packet []byte) ([]byte, error) {
	if len(packet) < spec.saltLen+ssTagSize {
		return nil, errors.New("shadowsocks datagram is too short")
	}
	salt := packet[:spec.saltLen]
	aead, err := ssAEAD(spec, key, salt)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, make([]byte, aead.NonceSize()), packet[spec.saltLen:], nil)
}

// ssStreamPacket encrypts one datagram for a stream cipher: [IV][ciphertext].
func ssStreamPacket(spec ssCipherSpec, key, payload []byte) ([]byte, error) {
	iv := make([]byte, spec.streamIVLen)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, err
	}
	stream, err := ssStream(spec, key, iv, true)
	if err != nil {
		return nil, err
	}
	out := make([]byte, spec.streamIVLen, spec.streamIVLen+len(payload))
	copy(out, iv)
	out = out[:spec.streamIVLen+len(payload)]
	stream.XORKeyStream(out[spec.streamIVLen:], payload)
	return out, nil
}

// ssStreamUnpack decrypts one datagram produced by ssStreamPacket.
func ssStreamUnpack(spec ssCipherSpec, key, packet []byte) ([]byte, error) {
	if len(packet) < spec.streamIVLen {
		return nil, errors.New("shadowsocks datagram is too short")
	}
	stream, err := ssStream(spec, key, packet[:spec.streamIVLen], false)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(packet)-spec.streamIVLen)
	stream.XORKeyStream(out, packet[spec.streamIVLen:])
	return out, nil
}

// ssConn is a Shadowsocks TCP tunnel. The address header and all payload data
// share one cipher stream: for AEAD methods the header is the first framed
// chunk, for stream methods it is the first plaintext after the IV.
type ssConn struct {
	net.Conn
	spec   ssCipherSpec
	key    []byte
	header []byte

	rMu sync.Mutex
	wMu sync.Mutex
	// buffered plaintext returned to the caller in slices.
	carry []byte
	// AEAD state; nil for stream ciphers.
	readAE  cipher.AEAD
	writeAE cipher.AEAD
	rnonce  []byte
	wnonce  []byte
	aeadOn  bool
	started bool
	// Stream cipher state; nil for AEAD methods. The two directions are
	// independent streams with their own IV, so they never share a cipher.Stream.
	readStream  cipher.Stream
	writeStream cipher.Stream
}

// newSSConn wraps c so that everything written after the first Write goes
// through the Shadowsocks tunnel. host:port is the requested destination.
func newSSConn(c net.Conn, method, password, host string, port int) (*ssConn, error) {
	spec, err := resolveCipherSpec(method)
	if err != nil {
		return nil, err
	}
	key, err := ssKey(spec.name, password)
	if err != nil {
		return nil, err
	}
	header, err := socksAddrHeader(host, port)
	if err != nil {
		return nil, err
	}
	return &ssConn{Conn: c, spec: spec, key: key, header: header, aeadOn: spec.aead}, nil
}

// Write sends b to the server. The destination header is prepended to the very
// first write so the server sees a single well-formed record sequence.
func (c *ssConn) Write(b []byte) (int, error) {
	c.wMu.Lock()
	defer c.wMu.Unlock()
	payload := b
	if !c.started {
		payload = make([]byte, 0, len(c.header)+len(b))
		payload = append(payload, c.header...)
		payload = append(payload, b...)
	}
	if err := c.writeLocked(payload); err != nil {
		return 0, err
	}
	c.started = true
	return len(b), nil
}

func (c *ssConn) writeLocked(payload []byte) error {
	if !c.aeadOn {
		if c.writeStream == nil {
			iv := make([]byte, c.spec.streamIVLen)
			if _, err := io.ReadFull(rand.Reader, iv); err != nil {
				return err
			}
			stream, err := ssStream(c.spec, c.key, iv, true)
			if err != nil {
				return err
			}
			if _, err := c.Conn.Write(iv); err != nil {
				return err
			}
			c.writeStream = stream
		}
		buf := make([]byte, len(payload))
		c.writeStream.XORKeyStream(buf, payload)
		_, err := c.Conn.Write(buf)
		return err
	}
	if c.writeAE == nil {
		salt := make([]byte, c.spec.saltLen)
		if _, err := io.ReadFull(rand.Reader, salt); err != nil {
			return err
		}
		aead, err := ssAEAD(c.spec, c.key, salt)
		if err != nil {
			return err
		}
		if _, err := c.Conn.Write(salt); err != nil {
			return err
		}
		c.writeAE = aead
		c.wnonce = make([]byte, aead.NonceSize())
	}
	for written := 0; written < len(payload); {
		chunk := len(payload) - written
		if chunk > ssMaxPayload {
			chunk = ssMaxPayload
		}
		lengthBytes := []byte{byte(chunk >> 8), byte(chunk)}
		header := c.writeAE.Seal(nil, c.wnonce, lengthBytes, nil)
		incrementNonce(c.wnonce)
		body := c.writeAE.Seal(nil, c.wnonce, payload[written:written+chunk], nil)
		incrementNonce(c.wnonce)
		frame := make([]byte, 0, len(header)+len(body))
		frame = append(frame, header...)
		frame = append(frame, body...)
		if _, err := c.Conn.Write(frame); err != nil {
			return err
		}
		written += chunk
	}
	return nil
}

// Read returns decrypted server bytes.
func (c *ssConn) Read(b []byte) (int, error) {
	c.rMu.Lock()
	defer c.rMu.Unlock()
	if !c.aeadOn {
		if c.readStream == nil {
			iv := make([]byte, c.spec.streamIVLen)
			if _, err := io.ReadFull(c.Conn, iv); err != nil {
				return 0, err
			}
			stream, err := ssStream(c.spec, c.key, iv, false)
			if err != nil {
				return 0, err
			}
			c.readStream = stream
		}
		n, err := c.Conn.Read(b)
		if n > 0 {
			c.readStream.XORKeyStream(b[:n], b[:n])
		}
		return n, err
	}
	for len(c.carry) == 0 {
		if err := c.readLocked(); err != nil {
			return 0, err
		}
	}
	n := copy(b, c.carry)
	c.carry = c.carry[n:]
	return n, nil
}

func (c *ssConn) readLocked() error {
	if c.readAE == nil {
		salt := make([]byte, c.spec.saltLen)
		if _, err := io.ReadFull(c.Conn, salt); err != nil {
			return err
		}
		aead, err := ssAEAD(c.spec, c.key, salt)
		if err != nil {
			return err
		}
		c.readAE = aead
		c.rnonce = make([]byte, aead.NonceSize())
	}
	header := make([]byte, 2+ssTagSize)
	if _, err := io.ReadFull(c.Conn, header); err != nil {
		return err
	}
	lengthBytes, err := c.readAE.Open(header[:0], c.rnonce, header, nil)
	if err != nil {
		return fmt.Errorf("shadowsocks length frame: %w", err)
	}
	incrementNonce(c.rnonce)
	if len(lengthBytes) != 2 {
		return errors.New("shadowsocks length frame is malformed")
	}
	size := int(binary.BigEndian.Uint16(lengthBytes))
	if size == 0 || size > ssMaxChunkSize {
		return fmt.Errorf("shadowsocks chunk size %d is invalid", size)
	}
	body := make([]byte, size+ssTagSize)
	if _, err := io.ReadFull(c.Conn, body); err != nil {
		return err
	}
	payload, err := c.readAE.Open(body[:0], c.rnonce, body, nil)
	if err != nil {
		return fmt.Errorf("shadowsocks payload frame: %w", err)
	}
	incrementNonce(c.rnonce)
	c.carry = payload
	return nil
}

// incrementNonce treats the AEAD nonce as a little-endian counter, matching the
// Shadowsocks AEAD specification.
func incrementNonce(nonce []byte) {
	for i := range nonce {
		nonce[i]++
		if nonce[i] != 0 {
			return
		}
	}
}

// socksAddrHeader encodes the destination as a SOCKS5 address.
func socksAddrHeader(host string, port int) ([]byte, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("destination port %d is invalid", port)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			out := make([]byte, 1+net.IPv4len+2)
			out[0] = 1
			copy(out[1:], ip4)
			binary.BigEndian.PutUint16(out[1+net.IPv4len:], uint16(port))
			return out, nil
		}
		out := make([]byte, 1+net.IPv6len+2)
		out[0] = 4
		copy(out[1:], ip.To16())
		binary.BigEndian.PutUint16(out[1+net.IPv6len:], uint16(port))
		return out, nil
	}
	if len(host) == 0 || len(host) > 255 {
		return nil, fmt.Errorf("destination host %q cannot be encoded", host)
	}
	out := make([]byte, 1+1+len(host)+2)
	out[0] = 3
	out[1] = byte(len(host))
	copy(out[2:], host)
	binary.BigEndian.PutUint16(out[2+len(host):], uint16(port))
	return out, nil
}

// decodeSocksAddrHeader parses a SOCKS5 address header and returns the host,
// port and total header length.
func decodeSocksAddrHeader(b []byte) (string, int, int, error) {
	if len(b) < 1 {
		return "", 0, 0, io.ErrUnexpectedEOF
	}
	switch b[0] {
	case 1:
		if len(b) < 1+net.IPv4len+2 {
			return "", 0, 0, io.ErrUnexpectedEOF
		}
		return net.IP(b[1 : 1+net.IPv4len]).String(),
			int(binary.BigEndian.Uint16(b[1+net.IPv4len:])), 1 + net.IPv4len + 2, nil
	case 4:
		if len(b) < 1+net.IPv6len+2 {
			return "", 0, 0, io.ErrUnexpectedEOF
		}
		return net.IP(b[1 : 1+net.IPv6len]).String(),
			int(binary.BigEndian.Uint16(b[1+net.IPv6len:])), 1 + net.IPv6len + 2, nil
	case 3:
		if len(b) < 2 {
			return "", 0, 0, io.ErrUnexpectedEOF
		}
		length := int(b[1])
		if len(b) < 2+length+2 {
			return "", 0, 0, io.ErrUnexpectedEOF
		}
		return string(b[2 : 2+length]),
			int(binary.BigEndian.Uint16(b[2+length:])), 2 + length + 2, nil
	}
	return "", 0, 0, errors.New("unsupported SOCKS address type")
}
