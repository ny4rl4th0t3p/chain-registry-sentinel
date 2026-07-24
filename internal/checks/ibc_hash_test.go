package checks_test

import (
	"testing"

	"chain-registry-sentinel/internal/checks"
	"chain-registry-sentinel/internal/registry"
)

func TestCheckDenomHashes_correct(t *testing.T) {
	// SHA256("transfer/channel-0/uatom") = 27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2
	// This is the well-known IBC ATOM denom on Osmosis mainnet.
	al := registry.AssetList{
		ChainName: "osmosis",
		Assets: []registry.IBCAsset{
			{
				Name: "Cosmos Hub Atom",
				Base: "ibc/27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2",
				Path: "transfer/channel-0/uatom",
			},
		},
	}
	mm := checks.CheckDenomHashes(al)
	if len(mm) != 0 {
		t.Errorf("want 0 mismatches for correct hash, got %d: %+v", len(mm), mm)
	}
}

func TestCheckDenomHashes_mismatch(t *testing.T) {
	al := registry.AssetList{
		ChainName: "osmosis",
		Assets: []registry.IBCAsset{
			{
				Name: "Cosmos Hub Atom",
				Base: "ibc/WRONGHASH",
				Path: "transfer/channel-0/uatom",
			},
		},
	}
	mm := checks.CheckDenomHashes(al)
	if len(mm) != 1 {
		t.Fatalf("want 1 mismatch, got %d", len(mm))
	}
	if mm[0].Base != "ibc/WRONGHASH" {
		t.Errorf("want Base=ibc/WRONGHASH, got %q", mm[0].Base)
	}
	if mm[0].Expected != "ibc/27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2" {
		t.Errorf("unexpected Expected: %q", mm[0].Expected)
	}
	if mm[0].Path != "transfer/channel-0/uatom" {
		t.Errorf("want Path=transfer/channel-0/uatom, got %q", mm[0].Path)
	}
	if mm[0].ChainName != "osmosis" {
		t.Errorf("want ChainName=osmosis, got %q", mm[0].ChainName)
	}
	if mm[0].AssetName != "Cosmos Hub Atom" {
		t.Errorf("want AssetName=Cosmos Hub Atom, got %q", mm[0].AssetName)
	}
}

func TestCheckDenomHashes_empty(t *testing.T) {
	al := registry.AssetList{ChainName: "osmosis"}
	mm := checks.CheckDenomHashes(al)
	if mm != nil {
		t.Errorf("want nil for empty asset list, got %v", mm)
	}
}

func TestCheckDenomHashes_multipleMismatches(t *testing.T) {
	al := registry.AssetList{
		ChainName: "osmosis",
		Assets: []registry.IBCAsset{
			{Name: "A", Base: "ibc/BAD1", Path: "transfer/channel-0/uatom"},
			{Name: "B", Base: "ibc/BAD2", Path: "transfer/channel-1/uosmo"},
			// Third asset has correct hash — should not appear in mismatches.
			{
				Name: "C",
				Base: "ibc/27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2",
				Path: "transfer/channel-0/uatom",
			},
		},
	}
	mm := checks.CheckDenomHashes(al)
	if len(mm) != 2 {
		t.Fatalf("want 2 mismatches, got %d", len(mm))
	}
}
