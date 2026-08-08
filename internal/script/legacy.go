package script

// Legacy (non-witness) opcode bytes used by the structural matchers below.
const (
	opDup           = 0x76
	opEqual         = 0x87
	opEqualVerify   = 0x88
	opHash160       = 0xa9
	opCheckSig      = 0xac
	opCheckMultisig = 0xae
	opReturn        = 0x6a
	opPushData1     = 0x4c
	opPushData2     = 0x4d
	opPushData4     = 0x4e
	op1Negate       = 0x4f

	hash160Len        = 20
	compressedPKLen   = 33
	uncompressedPKLen = 65
)

// matchP2PKH recognizes OP_DUP OP_HASH160 <20 bytes> OP_EQUALVERIFY
// OP_CHECKSIG (0x76 0xa9 0x14 <20> 0x88 0xac, 25 bytes total).
func matchP2PKH(s []byte) (pubKeyHash []byte, ok bool) {
	if len(s) != 25 {
		return nil, false
	}
	if s[0] != opDup || s[1] != opHash160 || s[2] != hash160Len ||
		s[23] != opEqualVerify || s[24] != opCheckSig {
		return nil, false
	}
	return s[3:23], true
}

// matchP2SH recognizes OP_HASH160 <20 bytes> OP_EQUAL
// (0xa9 0x14 <20> 0x87, 23 bytes total).
func matchP2SH(s []byte) (scriptHash []byte, ok bool) {
	if len(s) != 23 {
		return nil, false
	}
	if s[0] != opHash160 || s[1] != hash160Len || s[22] != opEqual {
		return nil, false
	}
	return s[2:22], true
}

// matchP2PK recognizes a bare compressed (33-byte) or uncompressed
// (65-byte) public key push followed by OP_CHECKSIG. This is the format
// QOGE genuinely used for coinbase outputs in blocks 1-7,985 before
// switching to P2PKH (confirmed against the live chain — see
// docs/ARCHITECTURE.md §7).
func matchP2PK(s []byte) (pubKey []byte, ok bool) {
	if len(s) == 1+compressedPKLen+1 && s[0] == compressedPKLen && s[len(s)-1] == opCheckSig {
		return s[1 : 1+compressedPKLen], true
	}
	if len(s) == 1+uncompressedPKLen+1 && s[0] == uncompressedPKLen && s[len(s)-1] == opCheckSig {
		return s[1 : 1+uncompressedPKLen], true
	}
	return nil, false
}

// matchNullData recognizes OP_RETURN followed by push-only data (or
// nothing). It does not enforce Core's "standardness" size limit — that is
// a relay-policy concern, not a structural classification concern.
func matchNullData(s []byte) bool {
	if len(s) == 0 || s[0] != opReturn {
		return false
	}
	return isPushOnly(s[1:])
}

// matchMultisig recognizes a bare m-of-n CHECKMULTISIG script:
//
//	OP_m <pubkey1> ... <pubkeyN> OP_n OP_CHECKMULTISIG
//
// where OP_m/OP_n are small-integer opcodes (1-16), each pubkey is a direct
// 33- or 65-byte push, n matches the actual number of pubkey pushes found,
// and m <= n. Returns the parsed pubkeys and the m/n threshold.
func matchMultisig(s []byte) (pubKeys [][]byte, m, n int, ok bool) {
	if len(s) < 3 || s[len(s)-1] != opCheckMultisig {
		return nil, 0, 0, false
	}
	m, ok = smallInt(s[0])
	if !ok {
		return nil, 0, 0, false
	}

	i := 1
	var keys [][]byte
	for i < len(s)-2 {
		pushLen := int(s[i])
		if pushLen != compressedPKLen && pushLen != uncompressedPKLen {
			break // not a pubkey push — must be the "n" byte
		}
		if i+1+pushLen > len(s)-2 {
			return nil, 0, 0, false // truncated
		}
		keys = append(keys, s[i+1:i+1+pushLen])
		i += 1 + pushLen
	}
	if i != len(s)-2 {
		return nil, 0, 0, false // trailing bytes before the "n" byte
	}

	n, ok = smallInt(s[i])
	if !ok {
		return nil, 0, 0, false
	}
	if m < 1 || n < m || n > 16 || len(keys) != n {
		return nil, 0, 0, false
	}
	return keys, m, n, true
}

// smallInt decodes a Bitcoin "small integer" push opcode (OP_0, OP_1-OP_16)
// into its numeric value.
func smallInt(b byte) (int, bool) {
	if b == opFalse {
		return 0, true
	}
	if b >= op1 && b <= op16 {
		return int(b-op1) + 1, true
	}
	return 0, false
}

// isPushOnly reports whether s consists entirely of valid, non-truncated
// data-push operations (OP_0, direct pushes 0x01-0x4b, OP_PUSHDATA1/2/4,
// OP_1NEGATE, OP_1-OP_16). Used to validate OP_RETURN payloads. Every branch
// is bounds-checked; malformed input returns false rather than panicking.
func isPushOnly(s []byte) bool {
	i := 0
	for i < len(s) {
		op := s[i]
		switch {
		case op == opFalse:
			i++
		case op >= 0x01 && op <= 0x4b:
			n := int(op)
			if i+1+n > len(s) {
				return false
			}
			i += 1 + n
		case op == opPushData1:
			if i+2 > len(s) {
				return false
			}
			n := int(s[i+1])
			if i+2+n > len(s) {
				return false
			}
			i += 2 + n
		case op == opPushData2:
			if i+3 > len(s) {
				return false
			}
			n := int(s[i+1]) | int(s[i+2])<<8
			if i+3+n > len(s) {
				return false
			}
			i += 3 + n
		case op == opPushData4:
			if i+5 > len(s) {
				return false
			}
			n := int(s[i+1]) | int(s[i+2])<<8 | int(s[i+3])<<16 | int(s[i+4])<<24
			if n < 0 || i+5+n > len(s) {
				return false
			}
			i += 5 + n
		case op == op1Negate:
			i++
		case op >= op1 && op <= op16:
			i++
		default:
			return false
		}
	}
	return true
}
