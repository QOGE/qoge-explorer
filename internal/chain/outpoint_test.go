package chain

import "testing"

func TestOutPoint_String(t *testing.T) {
	o := OutPoint{TxID: "046756d96716e5edce4f430daadef4d7d202eb2630e91745bc4830c55db5f4e6", Index: 0}
	want := "046756d96716e5edce4f430daadef4d7d202eb2630e91745bc4830c55db5f4e6:0"
	if got := o.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestOutPoint_Equality(t *testing.T) {
	a := OutPoint{TxID: "abc", Index: 1}
	b := OutPoint{TxID: "abc", Index: 1}
	c := OutPoint{TxID: "abc", Index: 2}
	d := OutPoint{TxID: "abd", Index: 1}

	if a != b {
		t.Errorf("identical OutPoints compared unequal: %+v != %+v", a, b)
	}
	if a == c {
		t.Errorf("OutPoints with different Index compared equal: %+v == %+v", a, c)
	}
	if a == d {
		t.Errorf("OutPoints with different TxID compared equal: %+v == %+v", a, d)
	}
}

// TestOutPoint_UsableAsMapKey confirms OutPoint's deterministic,
// comparable identity — required for later UTXO-set bookkeeping (§ task
// requirement B: "suitable for deterministic comparison/use").
func TestOutPoint_UsableAsMapKey(t *testing.T) {
	seen := map[OutPoint]bool{}
	op1 := OutPoint{TxID: "b3b13f313940e58dfac399a2ec8e758f750f38a81b40cc54d2e102caea3a3d8a", Index: 1}
	op2 := OutPoint{TxID: "dc077e573e540b627a559a11ff063a4b55e19d4b62030b7f46d7ab337fdc84ad", Index: 1}

	seen[op1] = true
	seen[op2] = true

	if !seen[OutPoint{TxID: "b3b13f313940e58dfac399a2ec8e758f750f38a81b40cc54d2e102caea3a3d8a", Index: 1}] {
		t.Error("expected map lookup by equal-but-distinct OutPoint value to hit")
	}
	if len(seen) != 2 {
		t.Errorf("expected 2 distinct map entries, got %d", len(seen))
	}
}
