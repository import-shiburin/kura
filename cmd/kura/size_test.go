package main

import "testing"

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"0":       0,
		"512":     512,
		"1KB":     1_000,
		"2.5TB":   2_500_000_000_000,
		"200GB":   200_000_000_000,
		"1MiB":    1 << 20,
		"1GiB":    1 << 30,
		"  4 GB ": 4_000_000_000,
	}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil {
			t.Errorf("parseSize(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "abc", "10XB", "GB"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) should have failed", bad)
		}
	}
}

// TestParseGenRejectsNonPositive keeps "at-gen 0" from printing an empty tree
// that looks like a real answer. Generation 0 is the state before anything was
// written.
func TestParseGenRejectsNonPositive(t *testing.T) {
	if got, err := parseGen("3"); err != nil || got != 3 {
		t.Errorf("parseGen(\"3\") = %d, %v", got, err)
	}
	for _, bad := range []string{"0", "-1", "", "two"} {
		if _, err := parseGen(bad); err == nil {
			t.Errorf("parseGen(%q) should have failed", bad)
		}
	}
}
