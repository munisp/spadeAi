package ids

import "testing"

func TestU128StableAndDistinct(t *testing.T) {
	a, b := U128("hold", "op1"), U128("hold", "op1")
	if a.Cmp(b) != 0 {
		t.Fatal("not deterministic")
	}
	if U128("hold", "op2").Cmp(a) == 0 || U128("ho", "ldop1").Cmp(a) == 0 {
		t.Fatal("collision across inputs")
	}
	if a.Sign() <= 0 || a.BitLen() > 128 {
		t.Fatal("out of TigerBeetle id range")
	}
}
