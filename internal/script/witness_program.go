package script

// Opcode bytes relevant to witness-program recognition. OP_0 through OP_16
// are Bitcoin/QOGE "small integer push" opcodes: OP_0 = 0x00, OP_1 = 0x51,
// ..., OP_16 = 0x60 (OP_1 + 15).
const (
	opFalse = 0x00 // OP_0 / OP_FALSE — witness version 0
	op1     = 0x51 // OP_1 — witness version 1
	op16    = 0x60 // OP_16 — witness version 16

	minWitnessProgramLen = 2  // BIP141/BIP350
	maxWitnessProgramLen = 40 // BIP141/BIP350
)

// WitnessProgram is a parsed SegWit-style witness program scriptPubKey:
// a witness version (0-16) and the program bytes it commits to.
type WitnessProgram struct {
	Version int
	Program []byte
}

// ParseWitnessProgram recognizes a scriptPubKey of the exact shape Core's
// CScript::IsWitnessProgram accepts:
//
//	<witness version opcode: OP_0 or OP_1..OP_16>
//	<a single direct data push of 2..40 bytes>
//	<nothing else>
//
// It never panics on malformed, truncated, or oversized input — every
// access is bounds-checked, and any input that isn't exactly this shape
// returns ok=false.
func ParseWitnessProgram(pkScript []byte) (wp WitnessProgram, ok bool) {
	// Minimum possible witness program script: 1 version byte + 1 length
	// byte + minWitnessProgramLen data bytes.
	if len(pkScript) < 2+minWitnessProgramLen {
		return WitnessProgram{}, false
	}
	// Maximum possible: 1 + 1 + maxWitnessProgramLen.
	if len(pkScript) > 2+maxWitnessProgramLen {
		return WitnessProgram{}, false
	}

	verByte := pkScript[0]
	var version int
	switch {
	case verByte == opFalse:
		version = 0
	case verByte >= op1 && verByte <= op16:
		version = int(verByte-op1) + 1
	default:
		return WitnessProgram{}, false
	}

	// The second byte must be a direct-push opcode whose value equals the
	// exact number of program bytes that follow (Bitcoin's push opcodes
	// 0x01-0x4b directly encode "push the next N bytes" for N == opcode
	// value; since the max witness program length is 40, this is always a
	// simple direct push, never OP_PUSHDATA1/2/4).
	pushLen := int(pkScript[1])
	if pushLen < minWitnessProgramLen || pushLen > maxWitnessProgramLen {
		return WitnessProgram{}, false
	}

	// The script must be exactly version-byte + length-byte + program —
	// no trailing garbage, nothing truncated.
	if len(pkScript) != 2+pushLen {
		return WitnessProgram{}, false
	}

	program := make([]byte, pushLen)
	copy(program, pkScript[2:])
	return WitnessProgram{Version: version, Program: program}, true
}
