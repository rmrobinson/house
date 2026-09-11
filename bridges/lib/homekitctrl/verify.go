package homekitctrl

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha512"
	"fmt"
	"io"
	"net"
	"net/http"
)

// pairVerifyResult holds the two directional session keys derived from a successful pair-verify
// exchange, ready to hand to newEncryptedConn.
type pairVerifyResult struct {
	controllerToAccessoryKey []byte
	accessoryToControllerKey []byte
}

// pairVerify performs HAP's pair-verify handshake (spec section 5.6) over conn, which must be a
// freshly dialed, not-yet-encrypted connection to the accessory. It authenticates the accessory
// against accessory.PublicKey (captured at pair-setup time) and proves our own identity using
// controller's long-term Ed25519 key.
//
// This is a from-scratch implementation rather than a dependency on an existing HAP client
// library: github.com/mctofu/homekit (the only maintained Go HAP controller client found) sends
// an extra, HAP-spec-invalid "Method" TLV tag on this exact request, which a real ecobee
// Smart Thermostat Premium flatly rejects with an empty-bodied HTTP 400 - confirmed by direct
// testing, not a hypothetical. That bug lives inside unexported dialer internals that can't be
// overridden from outside the library, so patching it means owning this whole layer either way.
func pairVerify(ctx context.Context, conn net.Conn, host string, controller *ControllerIdentity, accessory *AccessoryRecord) (*pairVerifyResult, error) {
	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ephemeral key: %w", err)
	}
	ourEphemeralPub := ephemeral.PublicKey().Bytes()

	// M1: State=1, our ephemeral Curve25519 public key. Deliberately no Method tag - see the doc
	// comment above.
	m1 := encodeTLV8(
		tlv8Item{tag: tlvTagState, value: []byte{1}},
		tlv8Item{tag: tlvTagPublicKey, value: ourEphemeralPub},
	)
	m2Body, err := postTLV8(ctx, conn, host, "/pair-verify", m1)
	if err != nil {
		return nil, fmt.Errorf("pair-verify M1: %w", err)
	}

	m2 := decodeTLV8(m2Body)
	if errCode, ok := m2[tlvTagError]; ok && len(errCode) > 0 {
		return nil, fmt.Errorf("pair-verify M2: accessory returned error code %d", errCode[0])
	}
	accessoryEphemeralPubBytes := m2[tlvTagPublicKey]
	encryptedData := m2[tlvTagEncryptedData]
	if len(accessoryEphemeralPubBytes) != 32 || len(encryptedData) == 0 {
		return nil, fmt.Errorf("pair-verify M2: malformed response (public key len %d, encrypted data len %d)", len(accessoryEphemeralPubBytes), len(encryptedData))
	}

	accessoryEphemeralPub, err := ecdh.X25519().NewPublicKey(accessoryEphemeralPubBytes)
	if err != nil {
		return nil, fmt.Errorf("pair-verify M2: invalid accessory public key: %w", err)
	}

	sharedSecret, err := ephemeral.ECDH(accessoryEphemeralPub)
	if err != nil {
		return nil, fmt.Errorf("pair-verify: ECDH: %w", err)
	}

	sessionKey, err := hkdf.Key(sha512.New, sharedSecret, []byte("Pair-Verify-Encrypt-Salt"), "Pair-Verify-Encrypt-Info", 32)
	if err != nil {
		return nil, fmt.Errorf("pair-verify: derive session key: %w", err)
	}

	plaintext, err := chachaOpen(sessionKey, "PV-Msg02", encryptedData)
	if err != nil {
		return nil, fmt.Errorf("pair-verify M2: decrypt: %w", err)
	}

	inner := decodeTLV8(plaintext)
	accessoryPairingID := inner[tlvTagIdentifier]
	accessorySignature := inner[tlvTagSignature]
	if len(accessoryPairingID) == 0 || len(accessorySignature) == 0 {
		return nil, fmt.Errorf("pair-verify M2: missing identifier or signature in encrypted payload")
	}
	if string(accessoryPairingID) != accessory.PairingID {
		return nil, fmt.Errorf("pair-verify M2: accessory identified as %q, expected %q", accessoryPairingID, accessory.PairingID)
	}

	accessorySignedMaterial := concat(accessoryEphemeralPubBytes, accessoryPairingID, ourEphemeralPub)
	if !ed25519.Verify(ed25519.PublicKey(accessory.PublicKey), accessorySignedMaterial, accessorySignature) {
		return nil, fmt.Errorf("pair-verify M2: accessory signature invalid - wrong pairing, or a different accessory answering this address")
	}

	// M3: prove our own identity the same way, encrypted under the same session key.
	ourSignedMaterial := concat(ourEphemeralPub, []byte(controller.PairingID), accessoryEphemeralPubBytes)
	ourSignature := ed25519.Sign(controller.PrivateKey, ourSignedMaterial)

	innerOut := encodeTLV8(
		tlv8Item{tag: tlvTagIdentifier, value: []byte(controller.PairingID)},
		tlv8Item{tag: tlvTagSignature, value: ourSignature},
	)
	sealed, err := chachaSeal(sessionKey, "PV-Msg03", innerOut)
	if err != nil {
		return nil, fmt.Errorf("pair-verify M3: encrypt: %w", err)
	}

	m3 := encodeTLV8(
		tlv8Item{tag: tlvTagState, value: []byte{3}},
		tlv8Item{tag: tlvTagEncryptedData, value: sealed},
	)
	m4Body, err := postTLV8(ctx, conn, host, "/pair-verify", m3)
	if err != nil {
		return nil, fmt.Errorf("pair-verify M3: %w", err)
	}

	m4 := decodeTLV8(m4Body)
	if errCode, ok := m4[tlvTagError]; ok && len(errCode) > 0 {
		return nil, fmt.Errorf("pair-verify M4: accessory returned error code %d", errCode[0])
	}

	controllerToAccessoryKey, err := hkdf.Key(sha512.New, sharedSecret, []byte("Control-Salt"), "Control-Write-Encryption-Key", 32)
	if err != nil {
		return nil, fmt.Errorf("pair-verify: derive write key: %w", err)
	}
	accessoryToControllerKey, err := hkdf.Key(sha512.New, sharedSecret, []byte("Control-Salt"), "Control-Read-Encryption-Key", 32)
	if err != nil {
		return nil, fmt.Errorf("pair-verify: derive read key: %w", err)
	}

	return &pairVerifyResult{
		controllerToAccessoryKey: controllerToAccessoryKey,
		accessoryToControllerKey: accessoryToControllerKey,
	}, nil
}

// postTLV8 writes a single TLV8-bodied POST request directly to conn and reads back the response
// body. Pair-verify's M1-M4 exchange must happen on one persistent, unencrypted TCP connection
// (the same connection is then wrapped in encryption and reused for the actual session), so this
// bypasses net/http's own connection pooling entirely rather than fighting it.
func postTLV8(ctx context.Context, conn net.Conn, host, path string, body []byte) ([]byte, error) {
	var respBody []byte

	err := withDeadline(ctx, conn, func() error {
		req, err := http.NewRequest(http.MethodPost, "http://"+host+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/pairing+tlv8")
		req.ContentLength = int64(len(body))

		if err := req.Write(conn); err != nil {
			return fmt.Errorf("write request: %w", err)
		}

		resp, err := http.ReadResponse(bufio.NewReader(conn), req)
		if err != nil {
			return fmt.Errorf("read response: %w", err)
		}
		defer resp.Body.Close()

		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("read response body: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(b))
		}
		respBody = b
		return nil
	})

	return respBody, err
}
