package script

// Classify determines the structural Type of a raw scriptPubKey.
//
// Classification is based entirely on pkScript's bytes. It never consults
// Core's RPC "type" string — there is no such parameter here — which
// trivially satisfies the requirement that RPC type strings must never be
// the sole (or any) basis for a P2QPK classification. Any RPC-provided
// "type"/"address" metadata is a concern for the future indexer layer that
// calls this package, not for Classify itself (see docs/ARCHITECTURE.md §9,
// "RPC classification is NOT source-distinguishable as P2QPK").
//
// Classify never panics, on any input, including nil, empty, truncated, or
// adversarially malformed scripts — every matcher it calls is fully
// bounds-checked and returns ok=false rather than indexing out of range.
func Classify(pkScript []byte) Result {
	if wp, ok := ParseWitnessProgram(pkScript); ok {
		v := wp.Version
		switch {
		case wp.Version == 0 && len(wp.Program) == hash160Len:
			return Result{Type: TypeP2WPKH, WitnessVersion: &v, WitnessProgram: wp.Program}
		case wp.Version == 0 && len(wp.Program) == 32:
			return Result{Type: TypeP2WSH, WitnessVersion: &v, WitnessProgram: wp.Program}
		case wp.Version == P2QPKWitnessVersion && len(wp.Program) == P2QPKProgramLength:
			// Structural P2QPK detection: witness version == 2 AND program
			// length == 32, exactly. This is the same rule Core's own
			// policy.cpp (AreInputsStandard) uses internally, confirmed
			// from source — see docs/ARCHITECTURE.md §9. Every other
			// version/length combination — including version 2 with any
			// length other than 32 — deliberately falls through to
			// TypeUnknownWitness below.
			return Result{Type: TypeP2QPK, WitnessVersion: &v, WitnessProgram: wp.Program}
		default:
			return Result{Type: TypeUnknownWitness, WitnessVersion: &v, WitnessProgram: wp.Program}
		}
	}

	// P2PKH/P2SH hashes aren't exposed on Result: Core's RPC already
	// supplies a ready-made address for these standard types, so there is
	// nothing for a later resolver to reconstruct (unlike P2PK/MULTISIG,
	// where Core deliberately omits the address — see docs/ARCHITECTURE.md
	// §7).
	if _, ok := matchP2PKH(pkScript); ok {
		return Result{Type: TypeP2PKH}
	}

	if _, ok := matchP2SH(pkScript); ok {
		return Result{Type: TypeP2SH}
	}

	if pubKey, ok := matchP2PK(pkScript); ok {
		return Result{Type: TypeP2PK, PubKeys: [][]byte{pubKey}}
	}

	if matchNullData(pkScript) {
		return Result{Type: TypeNullData}
	}

	if keys, m, n, ok := matchMultisig(pkScript); ok {
		return Result{Type: TypeMultisig, PubKeys: keys, MultisigM: m, MultisigN: n}
	}

	return Result{Type: TypeUnknown}
}
