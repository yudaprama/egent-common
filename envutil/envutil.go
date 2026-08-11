package envutil

import (
	"math/rand"
	"os"
	"strings"
)

// SplitCSV splits a comma-separated string, trims whitespace, and filters empty entries.
func SplitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// PickRandomKey reads a comma-separated env var and returns one key at random.
// Returns "" when the var is unset or empty.
func PickRandomKey(envName string) string {
	keys := SplitCSV(os.Getenv(envName))
	if len(keys) == 0 {
		return ""
	}
	return keys[rand.Intn(len(keys))]
}


