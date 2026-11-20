package application

import (
	"testing"

	"masamong/internal/providers/kma"
)

func TestDisasterWatchLevelMatrix(t *testing.T) {
	// Each row pairs the KMA severity with the first room level that includes it.
	for severity, kinds := range map[string]map[string]int{
		"2":    {"N": 2, "H": 4, "S": 4, "C": 4, "T": 4, "W": 5, "R": 5, "D": 5, "O": 5, "V": 5, "K": 5},
		"3":    {"N": 2, "H": 3, "S": 3, "C": 3, "T": 3, "R": 3, "O": 3, "W": 4, "Y": 4, "V": 4, "D": 5},
		"중대경보": {"H": 3},
	} {
		for kind, minimum := range kinds {
			w := kma.Warning{RegionCode: "L1052100", Kind: kind, Level: severity}
			for level := 1; level <= 5; level++ {
				ch := disasterChannel{WatchLevel: map[string]int{w.RegionCode: level}}
				if ch.watches(w) != (level >= minimum) {
					t.Errorf("kind %s, severity %s, room level %d: expected threshold %d", kind, severity, level, minimum)
				}
			}
		}
	}
}
