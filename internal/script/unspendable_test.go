package script

import "testing"

func TestIsUnspendable(t *testing.T) {
	tests := []struct {
		name   string
		script []byte
		want   bool
	}{
		{"nil script", nil, false},
		{"empty script", []byte{}, false},
		{"ordinary P2PKH", append([]byte{opDup, opHash160, 20}, append(fill(20, 1), opEqualVerify, opCheckSig)...), false},
		{"OP_RETURN with no data", []byte{opReturn}, true},
		{"OP_RETURN with data", []byte{opReturn, 0x04, 0xde, 0xad, 0xbe, 0xef}, true},
		{"script beginning with a push, not OP_RETURN", []byte{0x01, opReturn}, false},
		{"exactly MaxScriptSize bytes, not OP_RETURN", fill(MaxScriptSize, 1), false},
		{"MaxScriptSize + 1 bytes, not OP_RETURN", fill(MaxScriptSize+1, 1), true},
		{"OP_RETURN AND oversized (either condition alone suffices)", append([]byte{opReturn}, fill(MaxScriptSize, 1)...), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsUnspendable(tt.script); got != tt.want {
				t.Errorf("IsUnspendable(len=%d) = %v, want %v", len(tt.script), got, tt.want)
			}
		})
	}
}
