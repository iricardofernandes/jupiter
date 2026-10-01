package receivables

import (
	"testing"

	"github.com/iricardofernandes/jupiter/pkg/registryapi"
)

// The registry reduces a unit's free amount first, then its contracts from the latest
// accepted back (Convenção 3.14); Jupiter stops at its own.
func TestWaterfall(t *testing.T) {
	const jupiter = "11222333000181"
	p := registryapi.Position{Value: 10000, Free: 1000, Committed: []registryapi.Commitment{
		{Contract: "bank-1", Beneficiary: "33000167000101", Amount: 3000},
		{Contract: "jupiter-1", Beneficiary: jupiter, Amount: 4000},
		{Contract: "bank-2", Beneficiary: "60701190000104", Amount: 2000},
	}}
	for _, tt := range []struct {
		want, free int64
		contracts  map[string]int64
	}{
		{want: 500, free: 500, contracts: map[string]int64{}},
		{want: 2500, free: 1000, contracts: map[string]int64{"bank-2": 1500}},
		{want: 9000, free: 1000, contracts: map[string]int64{"bank-2": 2000}},
	} {
		free, contracts := waterfall(p, jupiter, tt.want)
		if free != tt.free || len(contracts) != len(tt.contracts) {
			t.Fatalf("reducing %d: %d free, %v", tt.want, free, contracts)
		}
		for c, x := range tt.contracts {
			if contracts[c] != x {
				t.Fatalf("reducing %d: %v, want %v", tt.want, contracts, tt.contracts)
			}
		}
	}
}
