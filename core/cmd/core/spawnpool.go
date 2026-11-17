package main

import (
	"fmt"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
)

// spawnSubPools is the sum of the standing characters' spawn sub-pools (limits.spawns.concurrent of each that has a
// spawns.allow), the share of MAX_DEFINITION_SESSIONS that is theirs (docs/adr/0084 §6).
func spawnSubPools(heads []definitions.Head) int {
	sum := 0
	for _, h := range heads {
		if h.Effective != nil {
			sum += h.Effective.SpawnSubPool()
		}
	}
	return sum
}

// checkSpawnPool refuses a MAX_DEFINITION_SESSIONS below the characters' sub-pools: a spawn counts in the definition
// pool, so a smaller pool would leave a sub-pool that can never be filled.
func checkSpawnPool(heads []definitions.Head, maxDefinition int) error {
	if sum := spawnSubPools(heads); sum > maxDefinition {
		return fmt.Errorf("MAX_DEFINITION_SESSIONS %d is below the sum %d of the characters' limits.spawns.concurrent (docs/adr/0084 §6)", maxDefinition, sum)
	}
	return nil
}
