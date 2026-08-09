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
//
// Mirrors Core's MatchPayToPubkey (src/script/standard.cpp), which requires
// not just the right push length but CPubKey::ValidSize on the pushed
// bytes — a correctly-prefixed key (0x02/0x03 for 33 bytes, 0x04/0x06/0x07
// for 65 bytes). isPubKeyPush already implements that check (shared with
// matchMultisig's pubkey validation); reused here rather than duplicated.
func matchP2PK(s []byte) (pubKey []byte, ok bool) {
	if len(s) == 1+compressedPKLen+1 && s[0] == compressedPKLen && s[len(s)-1] == opCheckSig {
		key := s[1 : 1+compressedPKLen]
		if !isPubKeyPush(key) {
			return nil, false
		}
		return key, true
	}
	if len(s) == 1+uncompressedPKLen+1 && s[0] == uncompressedPKLen && s[len(s)-1] == opCheckSig {
		key := s[1 : 1+uncompressedPKLen]
		if !isPubKeyPush(key) {
			return nil, false
		}
		return key, true
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

// maxPubKeysPerMultisig mirrors Qogecoin Core's MAX_PUBKEYS_PER_MULTISIG
// (src/script/script.h).
const maxPubKeysPerMultisig = 20

// scriptToken is one decoded opcode+operand, used only for structural bare-
// multisig parsing below — this is not a general script interpreter.
type scriptToken struct {
	opcode byte
	data   []byte // pushed data for push opcodes; nil for non-push opcodes
}

// nextToken decodes the single script element starting at s[i], mirroring
// enough of Core's CScript::GetOp to parse bare multisig scripts: OP_0,
// direct pushes (1-75 bytes), OP_PUSHDATA1/2/4, OP_1NEGATE, OP_1-OP_16, and
// any other single-byte opcode (e.g. OP_CHECKMULTISIG itself). Every branch
// is bounds-checked; returns ok=false rather than panicking on truncated or
// out-of-range input.
func nextToken(s []byte, i int) (tok scriptToken, next int, ok bool) {
	if i >= len(s) {
		return scriptToken{}, i, false
	}
	op := s[i]
	switch {
	case op == opFalse:
		return scriptToken{opcode: op, data: []byte{}}, i + 1, true
	case op >= 0x01 && op <= 0x4b:
		n := int(op)
		if i+1+n > len(s) {
			return scriptToken{}, i, false
		}
		return scriptToken{opcode: op, data: s[i+1 : i+1+n]}, i + 1 + n, true
	case op == opPushData1:
		if i+2 > len(s) {
			return scriptToken{}, i, false
		}
		n := int(s[i+1])
		if i+2+n > len(s) {
			return scriptToken{}, i, false
		}
		return scriptToken{opcode: op, data: s[i+2 : i+2+n]}, i + 2 + n, true
	case op == opPushData2:
		if i+3 > len(s) {
			return scriptToken{}, i, false
		}
		n := int(s[i+1]) | int(s[i+2])<<8
		if i+3+n > len(s) {
			return scriptToken{}, i, false
		}
		return scriptToken{opcode: op, data: s[i+3 : i+3+n]}, i + 3 + n, true
	case op == opPushData4:
		if i+5 > len(s) {
			return scriptToken{}, i, false
		}
		n := int(s[i+1]) | int(s[i+2])<<8 | int(s[i+3])<<16 | int(s[i+4])<<24
		if n < 0 || i+5+n > len(s) {
			return scriptToken{}, i, false
		}
		return scriptToken{opcode: op, data: s[i+5 : i+5+n]}, i + 5 + n, true
	default:
		// OP_1NEGATE, OP_1-OP_16, OP_CHECKMULTISIG, or anything else: a
		// plain single-byte opcode with no operand.
		return scriptToken{opcode: op}, i + 1, true
	}
}

// multisigScriptNumber decodes tok as a minimally-encoded script number in
// [min,max], mirroring Core's GetScriptNumber + CheckMinimalPush +
// CScriptNum as used by MatchMultisig (src/script/standard.cpp), bounded to
// the m/n domain of a bare multisig script (1..maxPubKeysPerMultisig).
//
// Values 1-16 must use the small-integer opcodes OP_1-OP_16. Values
// 17-maxPubKeysPerMultisig have no dedicated opcode and must instead be a
// minimal single-byte data push; per Core's CheckMinimalPush, a push
// encoding a value that a small-int opcode could have represented (1-16),
// or any non-single-byte encoding of a value in this bounded range, is
// rejected as non-minimal — real Bitcoin/QOGE script numbers in [1,20]
// never require more than one byte, so this narrow mirror is exact for this
// domain without needing a general CScriptNum implementation.
func multisigScriptNumber(tok scriptToken, min, max int) (int, bool) {
	if tok.opcode >= op1 && tok.opcode <= op16 {
		v := int(tok.opcode-op1) + 1
		if v < min || v > max {
			return 0, false
		}
		return v, true
	}
	if tok.opcode == 0x01 && len(tok.data) == 1 {
		v := int(tok.data[0])
		if v < 17 || v > maxPubKeysPerMultisig {
			// 1-16 (or 0, or -1) pushed as data instead of using the
			// dedicated opcode is non-minimal per Core's CheckMinimalPush.
			return 0, false
		}
		if v < min || v > max {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// isPubKeyPush mirrors Core's CPubKey::ValidSize (src/pubkey.h): a
// 0x02/0x03-prefixed 33-byte compressed key, or a 0x04/0x06/0x07-prefixed
// 65-byte uncompressed/hybrid key.
func isPubKeyPush(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	switch data[0] {
	case 0x02, 0x03:
		return len(data) == compressedPKLen
	case 0x04, 0x06, 0x07:
		return len(data) == uncompressedPKLen
	default:
		return false
	}
}

// matchMultisig recognizes a bare m-of-n CHECKMULTISIG script:
//
//	<m> <pubkey1> ... <pubkeyN> <n> OP_CHECKMULTISIG
//
// mirroring Core's MatchMultisig (src/script/standard.cpp) structurally:
// m and n may each be encoded either as a small-integer opcode (1-16) or,
// for 17-maxPubKeysPerMultisig, a minimal single-byte push; each pubkey
// must be a validly-sized compressed/uncompressed key push
// (isPubKeyPush); n must equal the actual number of pubkey pushes found,
// and 1 <= m <= n <= maxPubKeysPerMultisig. Returns the parsed pubkeys and
// the m/n threshold.
func matchMultisig(s []byte) (pubKeys [][]byte, m, n int, ok bool) {
	if len(s) < 3 || s[len(s)-1] != opCheckMultisig {
		return nil, 0, 0, false
	}

	mTok, i, tokOK := nextToken(s, 0)
	if !tokOK {
		return nil, 0, 0, false
	}
	m, ok = multisigScriptNumber(mTok, 1, maxPubKeysPerMultisig)
	if !ok {
		return nil, 0, 0, false
	}

	var keys [][]byte
	var nTok scriptToken
	for {
		tok, next, tokOK := nextToken(s, i)
		if !tokOK {
			return nil, 0, 0, false
		}
		if !isPubKeyPush(tok.data) {
			nTok = tok
			i = next
			break
		}
		keys = append(keys, tok.data)
		i = next
	}

	n, ok = multisigScriptNumber(nTok, m, maxPubKeysPerMultisig)
	if !ok {
		return nil, 0, 0, false
	}
	if len(keys) != n {
		return nil, 0, 0, false
	}
	// After consuming n's token, exactly the OP_CHECKMULTISIG byte already
	// confirmed at the top of this function must remain.
	if i != len(s)-1 {
		return nil, 0, 0, false
	}

	return keys, m, n, true
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
