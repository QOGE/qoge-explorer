// Package script classifies raw Qogecoin scriptPubKey bytes into an
// explicit, strongly-typed set of output kinds — structurally, from the
// script bytes themselves, never from Core's RPC "type" string alone.
//
// This matters most for P2QPK: Core's own Solver()/TxoutType classification
// has no dedicated P2QPK entry (confirmed from Qogecoin Core source — see
// docs/ARCHITECTURE.md §9), so any witness version 2 output — including a
// real P2QPK output — is reported by RPC as generic "witness_unknown".
// Classify never trusts that string; it inspects the witness version and
// program length directly, exactly mirroring Core's own internal policy
// check (src/policy/policy.cpp, AreInputsStandard).
package script

// Type is a strongly-typed scriptPubKey classification. Using a defined
// string type (rather than bare strings scattered through the codebase)
// makes every classification site type-checked and greppable.
type Type string

const (
	TypeP2PK           Type = "p2pk"
	TypeP2PKH          Type = "p2pkh"
	TypeP2SH           Type = "p2sh"
	TypeP2WPKH         Type = "p2wpkh"
	TypeP2WSH          Type = "p2wsh"
	TypeP2QPK          Type = "p2qpk"
	TypeNullData       Type = "nulldata"
	TypeMultisig       Type = "multisig"
	TypeUnknownWitness Type = "unknown_witness" // a structurally valid witness program, but not one of the recognized version/length combinations above
	TypeUnknown        Type = "unknown"         // not a witness program, and does not match any recognized legacy template
)

// P2QPK structural parameters, confirmed from Qogecoin Core source
// (src/script/interpreter.h) and cross-checked against the Symbiont Wallet
// implementation (docs/ARCHITECTURE.md §12) — zero disagreements found.
//
// These are display/validation metadata only. This package does not sign or
// verify SLH-DSA signatures, does not import Symbiont Wallet's signer
// package, and takes no liboqs/CGo dependency.
const (
	// P2QPKWitnessVersion is the SegWit witness version used by P2QPK
	// (SigVersion::WITNESS_V2_SLHDSA in Core).
	P2QPKWitnessVersion = 2

	// P2QPKProgramLength is the exact byte length of a P2QPK witness
	// program. The program is HASH256(pubkey), a commitment — NOT the raw
	// public key — but happens to also be 32 bytes, the same length as the
	// key it commits to.
	P2QPKProgramLength = 32

	// P2QPKPublicKeyLength is the exact byte length of an SLH-DSA-SHA2-128f
	// public key, as revealed in the witness stack at spend time.
	P2QPKPublicKeyLength = 32

	// P2QPKSignatureLength is the exact byte length of an SLH-DSA-SHA2-128f
	// signature (FIPS 205, "f" fast parameter set). This is an exact
	// requirement, not a maximum — SLH-DSA has no variable-length fields.
	P2QPKSignatureLength = 17088
)

// Result is the outcome of classifying a single scriptPubKey.
type Result struct {
	Type Type

	// WitnessVersion and WitnessProgram are populated whenever the script is
	// a structurally valid witness program (P2WPKH, P2WSH, P2QPK, or
	// TypeUnknownWitness). Nil/empty for legacy (non-witness) script types.
	WitnessVersion *int
	WitnessProgram []byte

	// PubKeys holds the raw public key(s) found in a P2PK (one key) or bare
	// MULTISIG (all participant keys) scriptPubKey, for a later address
	// resolver to consume. Classify never derives or invents an address
	// itself — see docs/ARCHITECTURE.md §7. Nil for every other type.
	PubKeys [][]byte

	// MultisigM and MultisigN are the "m-of-n" threshold for TypeMultisig
	// results. Zero for every other type.
	MultisigM int
	MultisigN int
}
