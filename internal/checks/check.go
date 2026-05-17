package checks

import (
	"context"

	"chain-registry-sentinel/internal/registry"
)

type Result struct {
	Chain    string
	Check    string
	Endpoint string
	Passed   bool
	Evidence string
}

type Check interface {
	Name() string
	Run(ctx context.Context, chain registry.Chain) []Result
}
