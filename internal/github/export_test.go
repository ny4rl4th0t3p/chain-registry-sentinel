package github

import "chain-registry-sentinel/internal/registry"

// ForTest returns a shallow copy of c with a custom base URL (test helper only).
func (c *Client) ForTest(baseURL string) *Client {
	return c.withBaseURL(baseURL)
}

// EditChainJSON exposes editChainJSON for external tests.
func EditChainJSON(registryPath, chainName string, dead []FlaggedEndpoint) ([]byte, error) {
	return editChainJSON(registryPath, chainName, dead)
}

// BuildPRBody exposes buildPRBody for external tests.
func BuildPRBody(chain registry.Chain, dead []FlaggedEndpoint) string {
	return buildPRBody(chain, dead)
}
