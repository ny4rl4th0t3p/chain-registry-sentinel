package registry_test

import (
	"os"
	"path/filepath"
	"testing"

	"chain-registry-sentinel/internal/registry"
)

func writeAssetListJSON(t *testing.T, dir, chainName, content string) {
	t.Helper()
	chainDir := filepath.Join(dir, chainName)
	if err := os.MkdirAll(chainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chainDir, "assetlist.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAssetList_MissingFile(t *testing.T) {
	dir := t.TempDir()

	al, err := registry.LoadAssetList(dir, "nochain")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if al.ChainName != "nochain" {
		t.Errorf("want chain name nochain, got %q", al.ChainName)
	}
	if len(al.Assets) != 0 {
		t.Errorf("want 0 assets, got %d", len(al.Assets))
	}
}

func TestLoadAssetList_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	writeAssetListJSON(t, dir, "badchain", `not json`)

	_, err := registry.LoadAssetList(dir, "badchain")
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestLoadAssetList_FiltersNonIBC(t *testing.T) {
	dir := t.TempDir()
	writeAssetListJSON(t, dir, "osmosis", `{
		"chain_name": "osmosis",
		"assets": [
			{"name": "Osmosis", "base": "uosmo"},
			{
				"name": "Cosmos Hub Atom",
				"base": "ibc/27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2",
				"traces": [
					{"type": "ibc", "chain": {"path": "transfer/channel-0/uatom"}}
				]
			}
		]
	}`)

	al, err := registry.LoadAssetList(dir, "osmosis")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(al.Assets) != 1 {
		t.Fatalf("want 1 IBC asset, got %d", len(al.Assets))
	}
	a := al.Assets[0]
	if a.Name != "Cosmos Hub Atom" {
		t.Errorf("want name Cosmos Hub Atom, got %q", a.Name)
	}
	if a.Base != "ibc/27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2" {
		t.Errorf("unexpected base %q", a.Base)
	}
	if a.Path != "transfer/channel-0/uatom" {
		t.Errorf("want path transfer/channel-0/uatom, got %q", a.Path)
	}
}

func TestLoadAssetList_MultiHopUsesLastTracePath(t *testing.T) {
	dir := t.TempDir()
	// Two IBC hops: the last trace carries the full accumulated path.
	writeAssetListJSON(t, dir, "junction", `{
		"chain_name": "junction",
		"assets": [
			{
				"name": "Two-hop Atom",
				"base": "ibc/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
				"traces": [
					{"type": "ibc", "chain": {"path": "transfer/channel-0/uatom"}},
					{"type": "ibc", "chain": {"path": "transfer/channel-1/transfer/channel-0/uatom"}}
				]
			}
		]
	}`)

	al, err := registry.LoadAssetList(dir, "junction")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(al.Assets) != 1 {
		t.Fatalf("want 1 asset, got %d", len(al.Assets))
	}
	if got := al.Assets[0].Path; got != "transfer/channel-1/transfer/channel-0/uatom" {
		t.Errorf("want full accumulated path, got %q", got)
	}
}

func TestLoadAssetList_LastTraceWithoutPathFallsBack(t *testing.T) {
	dir := t.TempDir()
	// A non-IBC trace appended after the IBC trace has no chain.path;
	// the loader walks backward to the last trace that has one.
	writeAssetListJSON(t, dir, "wrapped", `{
		"chain_name": "wrapped",
		"assets": [
			{
				"name": "Bridged Token",
				"base": "ibc/BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
				"traces": [
					{"type": "ibc-cw20", "chain": {"path": "transfer/channel-2/cw20:juno1abc"}},
					{"type": "additional-mintage", "chain": {}}
				]
			}
		]
	}`)

	al, err := registry.LoadAssetList(dir, "wrapped")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(al.Assets) != 1 {
		t.Fatalf("want 1 asset, got %d", len(al.Assets))
	}
	if got := al.Assets[0].Path; got != "transfer/channel-2/cw20:juno1abc" {
		t.Errorf("want path from ibc-cw20 trace, got %q", got)
	}
}

func TestLoadAssetList_SkipsIBCAssetWithoutPath(t *testing.T) {
	dir := t.TempDir()
	// An ibc/ base with no trace path cannot be verified — it must be skipped,
	// not reported as a mismatch.
	writeAssetListJSON(t, dir, "sparse", `{
		"chain_name": "sparse",
		"assets": [
			{
				"name": "Untraceable",
				"base": "ibc/CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
			},
			{
				"name": "Traceable Atom",
				"base": "ibc/27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2",
				"traces": [
					{"type": "ibc", "chain": {"path": "transfer/channel-0/uatom"}}
				]
			}
		]
	}`)

	al, err := registry.LoadAssetList(dir, "sparse")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(al.Assets) != 1 {
		t.Fatalf("want 1 asset, got %d", len(al.Assets))
	}
	if al.Assets[0].Name != "Traceable Atom" {
		t.Errorf("want Traceable Atom, got %q", al.Assets[0].Name)
	}
}
