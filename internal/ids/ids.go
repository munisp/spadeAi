// Package ids derives deterministic TigerBeetle u128 identifiers.
package ids

import (
	"crypto/sha256"
	"math/big"
	"strings"
)

// U128 returns a non-zero, non-max 128-bit id derived from the given parts.
func U128(parts ...string) *big.Int {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	n := new(big.Int).SetBytes(sum[:16])
	if n.Sign() == 0 {
		n.SetInt64(1)
	}
	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	if n.Cmp(max) == 0 {
		n.Sub(n, big.NewInt(1))
	}
	return n
}
