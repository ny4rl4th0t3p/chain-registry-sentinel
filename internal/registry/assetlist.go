package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IBCAsset represents a single IBC-bridged asset from an assetlist.json.
type IBCAsset struct {
	Name string // display name
	Base string // "ibc/HASH" as declared in the file
	Path string // full denom trace path — the pre-image of the hash
}

// AssetList holds the IBC assets parsed from a chain's assetlist.json.
type AssetList struct {
	ChainName string
	Assets    []IBCAsset
}

type assetListJSON struct {
	ChainName string      `json:"chain_name"`
	Assets    []assetJSON `json:"assets"`
}

type assetJSON struct {
	Name   string      `json:"name"`
	Base   string      `json:"base"`
	Traces []traceJSON `json:"traces"`
}

type traceJSON struct {
	Type  string `json:"type"`
	Chain struct {
		Path string `json:"path"`
	} `json:"chain"`
}

// LoadAssetList reads {registryPath}/{chainName}/assetlist.json and returns
// only IBC assets (base starts with "ibc/"). Returns an empty AssetList when
// the file does not exist.
func LoadAssetList(registryPath, chainName string) (AssetList, error) {
	p := filepath.Join(registryPath, chainName, "assetlist.json")
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return AssetList{ChainName: chainName}, nil
		}
		return AssetList{}, fmt.Errorf("load assetlist %s: %w", p, err)
	}
	var raw assetListJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return AssetList{}, fmt.Errorf("parse assetlist %s: %w", p, err)
	}
	al := AssetList{ChainName: chainName}
	for _, a := range raw.Assets {
		if !strings.HasPrefix(a.Base, "ibc/") {
			continue
		}
		tp := lastTracePath(a.Traces)
		if tp == "" {
			continue
		}
		al.Assets = append(al.Assets, IBCAsset{
			Name: a.Name,
			Base: a.Base,
			Path: tp,
		})
	}
	return al, nil
}

// lastTracePath returns the chain.path from the last trace that has one set.
// The last IBC-type trace always carries the full accumulated path string,
// covering both single-hop and multi-hop cases.
func lastTracePath(traces []traceJSON) string {
	for i := len(traces) - 1; i >= 0; i-- {
		if traces[i].Chain.Path != "" {
			return traces[i].Chain.Path
		}
	}
	return ""
}
