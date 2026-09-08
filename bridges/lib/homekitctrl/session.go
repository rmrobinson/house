package homekitctrl

import (
	"bytes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"golang.org/x/crypto/chacha20poly1305"
)

// maxFramePlaintext is HAP's maximum plaintext size per encrypted frame (spec section 5.5.2).
const maxFramePlaintext = 1024

// encryptedConn wraps a net.Conn with HAP's post-pair-verify session encryption. Each direction
// uses its own key and its own independently incrementing nonce counter; plaintext is split into
// <=1024-byte frames, each prefixed with a 2-byte little-endian length that also serves as the
// AEAD's associated data.
type encryptedConn struct {
	net.Conn

	writeAEAD    cipher.AEAD
	writeCounter uint64

	readAEAD    cipher.AEAD
	readCounter uint64
	readBuf     bytes.Buffer // decrypted bytes read from the wire but not yet consumed
}

// newEncryptedConn wraps conn. writeKey encrypts data sent to the accessory (the controller's
// "Control-Write-Encryption-Key"); readKey decrypts data received from it
// ("Control-Read-Encryption-Key").
func newEncryptedConn(conn net.Conn, writeKey, readKey []byte) (*encryptedConn, error) {
	w, err := chacha20poly1305.New(writeKey)
	if err != nil {
		return nil, fmt.Errorf("init write cipher: %w", err)
	}
	r, err := chacha20poly1305.New(readKey)
	if err != nil {
		return nil, fmt.Errorf("init read cipher: %w", err)
	}
	return &encryptedConn{Conn: conn, writeAEAD: w, readAEAD: r}, nil
}

// frameNonce builds HAP's per-frame nonce: 4 zero bytes followed by an 8-byte little-endian
// frame counter, incrementing independently per direction.
func frameNonce(counter uint64) []byte {
	nonce := make([]byte, chacha20poly1305.NonceSize)
	binary.LittleEndian.PutUint64(nonce[4:], counter)
	return nonce
}

func (c *encryptedConn) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxFramePlaintext {
			chunk = chunk[:maxFramePlaintext]
		}

		var lengthPrefix [2]byte
		binary.LittleEndian.PutUint16(lengthPrefix[:], uint16(len(chunk)))

		sealed := c.writeAEAD.Seal(nil, frameNonce(c.writeCounter), chunk, lengthPrefix[:])
		c.writeCounter++

		if _, err := c.Conn.Write(append(lengthPrefix[:], sealed...)); err != nil {
			return total, err
		}

		total += len(chunk)
		p = p[len(chunk):]
	}
	return total, nil
}

func (c *encryptedConn) Read(p []byte) (int, error) {
	if c.readBuf.Len() == 0 {
		if err := c.readFrame(); err != nil {
			return 0, err
		}
	}
	return c.readBuf.Read(p)
}

func (c *encryptedConn) readFrame() error {
	var lengthPrefix [2]byte
	if _, err := io.ReadFull(c.Conn, lengthPrefix[:]); err != nil {
		return err
	}
	plainLen := int(binary.LittleEndian.Uint16(lengthPrefix[:]))

	sealed := make([]byte, plainLen+c.readAEAD.Overhead())
	if _, err := io.ReadFull(c.Conn, sealed); err != nil {
		return err
	}

	plain, err := c.readAEAD.Open(nil, frameNonce(c.readCounter), sealed, lengthPrefix[:])
	if err != nil {
		return fmt.Errorf("decrypt frame: %w", err)
	}
	c.readCounter++

	c.readBuf.Write(plain)
	return nil
}
