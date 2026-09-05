package main

import (
	"fmt"
	"strconv"
	"strings"
)

func mustSize(s, flagName string) int64 {
	n, err := parseSize(s)
	if err != nil {
		fatal(fmt.Errorf("%s: %w", flagName, err))
	}
	return n
}

func mustHeadroom(s string, capacity int64) int64 {
	if num, ok := strings.CutSuffix(s, "%"); ok {
		v, err := strconv.ParseFloat(num, 64)
		if err != nil {
			fatal(fmt.Errorf("-headroom: %w", err))
		}
		return int64(float64(capacity) * v / 100)
	}
	return mustSize(s, "-headroom")
}

// parseSize accepts plain byte counts and both decimal (GB, TB) and binary
// (GiB, TiB) suffixes. Media capacities are quoted in decimal units, so a bare
// "GB" means 10^9.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	v, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, fmt.Errorf("bad number in %q", s)
	}
	var mult float64
	switch strings.ToUpper(strings.TrimSpace(s[i:])) {
	case "", "B":
		mult = 1
	case "K", "KB":
		mult = 1e3
	case "M", "MB":
		mult = 1e6
	case "G", "GB":
		mult = 1e9
	case "T", "TB":
		mult = 1e12
	case "KIB":
		mult = 1 << 10
	case "MIB":
		mult = 1 << 20
	case "GIB":
		mult = 1 << 30
	case "TIB":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("unknown unit in %q", s)
	}
	return int64(v * mult), nil
}

// parseGen reads a generation number, which must be positive: generation 0 is
// the state before anything was written and has no tree to show.
func parseGen(s string) (int, error) {
	g, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("generation %q is not a number", s)
	}
	if g < 1 {
		return 0, fmt.Errorf("generation must be 1 or greater, got %d", g)
	}
	return g, nil
}
