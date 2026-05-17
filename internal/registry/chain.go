package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Endpoint struct {
	Address  string `json:"address"`
	Provider string `json:"provider"`
}

type Chain struct {
	Name    string
	ChainID string
	RPCs    []Endpoint
}

type chainJSON struct {
	ChainName string `json:"chain_name"`
	ChainID   string `json:"chain_id"`
	APIs      struct {
		RPC []Endpoint `json:"rpc"`
	} `json:"apis"`
}

// LoadChains reads all chain directories under registryPath. If filter is
// non-empty, only chains whose name appears in the list are returned.
func LoadChains(registryPath string, filter []string) ([]Chain, error) {
	entries, err := os.ReadDir(registryPath)
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}

	filterSet := make(map[string]bool, len(filter))
	for _, name := range filter {
		filterSet[strings.TrimSpace(name)] = true
	}

	var chains []Chain
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		if len(filterSet) > 0 && !filterSet[name] {
			continue
		}

		chainFile := filepath.Join(registryPath, name, "chain.json")
		data, err := os.ReadFile(chainFile)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", chainFile, err)
		}

		var cj chainJSON
		if err := json.Unmarshal(data, &cj); err != nil {
			return nil, fmt.Errorf("parse %s: %w", chainFile, err)
		}

		chains = append(chains, Chain{
			Name:    cj.ChainName,
			ChainID: cj.ChainID,
			RPCs:    cj.APIs.RPC,
		})
	}
	return chains, nil
}
