package main

import (
	"strings"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
)

func TestCheckSpawnPool(t *testing.T) {
	char := func(concurrent int) definitions.Head {
		return definitions.Head{Effective: &definitions.Spec{
			Spawns: &definitions.AllowList{Allow: []string{"w"}},
			Limits: &definitions.Limits{Spawns: &definitions.SpawnLimits{Concurrent: concurrent}},
		}}
	}
	noSpawn := definitions.Head{Effective: &definitions.Spec{}}
	heads := []definitions.Head{char(2), char(0), noSpawn, {}}
	// 2 + the default 1 = 3.
	if got := spawnSubPools(heads); got != 3 {
		t.Fatalf("sum %d, want 3", got)
	}
	if err := checkSpawnPool(heads, 3); err != nil {
		t.Fatalf("a pool equal to the sum is allowed: %v", err)
	}
	if err := checkSpawnPool(heads, 2); err == nil || !strings.Contains(err.Error(), "below the sum 3") {
		t.Fatalf("a pool below the sum must be refused, got %v", err)
	}
	if err := checkSpawnPool(nil, 0); err != nil {
		t.Fatalf("no characters, no sum: %v", err)
	}
}
