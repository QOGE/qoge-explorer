package script

// MaxScriptSize mirrors Qogecoin Core's MAX_SCRIPT_SIZE (src/script/script.h):
// scriptPubKeys longer than this are defined unspendable by Core's own
// coin-view logic, independent of their content.
const MaxScriptSize = 10000

// IsUnspendable mirrors Core's CScript::IsUnspendable() (src/script/script.h)
// exactly:
//
//	(size() > 0 && first opcode == OP_RETURN) || size() > MAX_SCRIPT_SIZE
//
// Core's CCoinsViewCache::AddCoin checks this and, if true, never adds the
// output to the coins view at all — an unspendable output is not a missing
// transaction output (the raw scriptPubKey remains immutable on-chain
// history), it is simply one with no canonical coin/UTXO row. This is a
// structural, byte-level check on the raw script — it deliberately does not
// consult ScriptType/Classify: Core's rule looks only at pkScript[0] and
// len(pkScript), so a NON-nulldata script longer than MaxScriptSize is
// unspendable too, even though Classify has no dedicated "too large" type
// for it.
//
// IsUnspendable never panics, on any input including nil or empty.
func IsUnspendable(pkScript []byte) bool {
	if len(pkScript) > 0 && pkScript[0] == opReturn {
		return true
	}
	return len(pkScript) > MaxScriptSize
}
