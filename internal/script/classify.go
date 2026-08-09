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
		case wp.Version == 0 && len(wp.Program) == p2wshProgramLength:
			return Result{Type: TypeP2WSH, WitnessVersion: &v, WitnessProgram: wp.Program}
		case wp.Version == p2trWitnessVersion && len(wp.Program) == p2trProgramLength:
			// Mirrors Core's Solver(): witnessversion == 1 && program.size()
			// == 32 -> TxoutType::WITNESS_V1_TAPROOT (src/script/standard.cpp).
			// A real QOGE Taproot output exists on-chain at height 1,284,510,
			// which is also used as a P2QPK negative test — see classify_test.go.
			return Result{Type: TypeP2TR, WitnessVersion: &v, WitnessProgram: wp.Program}
		case wp.Version == P2QPKWitnessVersion && len(wp.Program) == P2QPKProgramLength:
			// Structural P2QPK detection: witness version == 2 AND program
			// length == 32, exactly. This is the same rule Core's own
			// policy.cpp (AreInputsStandard) uses internally, confirmed
			// from source — see docs/ARCHITECTURE.md §9. Every other
			// version/length combination — including version 2 with any
			// length other than 32 — deliberately falls through below.
			return Result{Type: TypeP2QPK, WitnessVersion: &v, WitnessProgram: wp.Program}
		case wp.Version == 0:
			// Confirmed from Core source (src/script/standard.cpp, Solver()):
			// witnessversion == 0 with a length other than 20 or 32 falls
			// through to TxoutType::NONSTANDARD, NOT WITNESS_UNKNOWN — v0 is
			// a fully-defined BIP141 version with only those two valid
			// lengths, so an off-length v0 program isn't "an unrecognized
			// witness version," it's simply not a standard output at all.
			// Mapped to TypeUnknown here, deliberately not TypeUnknownWitness.
			return Result{Type: TypeUnknown, WitnessVersion: &v, WitnessProgram: wp.Program}
		default:
			// witnessversion != 0 and not one of the recognized
			// version/length combinations above -> Core's
			// TxoutType::WITNESS_UNKNOWN.
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
