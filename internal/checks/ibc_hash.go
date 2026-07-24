package checks

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"chain-registry-sentinel/internal/registry"
)

// HashMismatch describes an IBC asset whose declared base hash does not match
// the SHA256 of its trace path.
type HashMismatch struct {
	ChainName string
	AssetName string
	Base      string // as declared in file: "ibc/WRONG"
	Expected  string // "ibc/" + uppercase hex SHA256(path)
	Path      string
}

// CheckDenomHashes recomputes ibc/<SHA256(path)> for every asset in al and
// returns entries where the declared Base does not match.
func CheckDenomHashes(al registry.AssetList) []HashMismatch {
	var out []HashMismatch
	for _, a := range al.Assets {
		sum := sha256.Sum256([]byte(a.Path))
		expected := "ibc/" + strings.ToUpper(fmt.Sprintf("%x", sum))
		if a.Base != expected {
			out = append(out, HashMismatch{
				ChainName: al.ChainName,
				AssetName: a.Name,
				Base:      a.Base,
				Expected:  expected,
				Path:      a.Path,
			})
		}
	}
	return out
}
