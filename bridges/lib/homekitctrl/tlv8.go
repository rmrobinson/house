package homekitctrl

// Minimal TLV8 (tag-length-value, 1-byte tag + 1-byte length) encode/decode, as used by HAP's
// pairing and pair-verify exchanges. See HAP spec section 5.6.

const (
	tlvTagIdentifier    = 0x01 // aka "Username" in some HAP docs
	tlvTagPublicKey     = 0x03
	tlvTagEncryptedData = 0x05
	tlvTagState         = 0x06
	tlvTagError         = 0x07
	tlvTagSignature     = 0x0A
)

type tlv8Item struct {
	tag   byte
	value []byte
}

// encodeTLV8 concatenates items in order. Per spec a value over 255 bytes must be split into
// consecutive same-tag chunks, but nothing this package sends (state bytes, keys, signatures,
// short encrypted blobs) is anywhere near that large, so this doesn't implement splitting.
func encodeTLV8(items ...tlv8Item) []byte {
	var buf []byte
	for _, it := range items {
		if len(it.value) > 255 {
			panic("homekitctrl: tlv8 value exceeds 255 bytes, chunking not implemented")
		}
		buf = append(buf, it.tag, byte(len(it.value)))
		buf = append(buf, it.value...)
	}
	return buf
}

// decodeTLV8 parses a flat TLV8 buffer into a tag->value map, concatenating consecutive entries
// that share the same tag (the spec's convention for values that were split into >255-byte
// chunks by the sender).
func decodeTLV8(data []byte) map[byte][]byte {
	result := make(map[byte][]byte)

	var lastTag byte
	hasLast := false

	for i := 0; i+2 <= len(data); {
		tag := data[i]
		length := int(data[i+1])
		i += 2

		if i+length > len(data) {
			break
		}
		value := data[i : i+length]
		i += length

		if hasLast && tag == lastTag {
			result[tag] = append(result[tag], value...)
		} else {
			buf := make([]byte, len(value))
			copy(buf, value)
			result[tag] = buf
		}
		lastTag = tag
		hasLast = true
	}

	return result
}
