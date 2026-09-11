package homekitctrl

import "golang.org/x/crypto/chacha20poly1305"

// concat returns the concatenation of parts as a single new slice, used to build the exact byte
// sequences HAP signs/verifies during pair-verify (e.g. ephemeral key || pairing ID || peer's
// ephemeral key).
func concat(parts ...[]byte) []byte {
	var n int
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// fixedNonce builds a ChaCha20-Poly1305 nonce from HAP's fixed-message convention: 4 zero bytes
// followed by the 8-byte ASCII message name (e.g. "PV-Msg02"), used for the handful of
// single-shot encrypted TLV8 payloads exchanged during pairing/pair-verify. Per-frame session
// traffic after pair-verify uses an incrementing counter instead - see frameNonce in session.go.
func fixedNonce(name string) []byte {
	if len(name) != 8 {
		panic("homekitctrl: fixed nonce name must be 8 bytes")
	}
	nonce := make([]byte, chacha20poly1305.NonceSize)
	copy(nonce[4:], name)
	return nonce
}

func chachaSeal(key []byte, nonceName string, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, fixedNonce(nonceName), plaintext, nil), nil
}

func chachaOpen(key []byte, nonceName string, ciphertext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, fixedNonce(nonceName), ciphertext, nil)
}
