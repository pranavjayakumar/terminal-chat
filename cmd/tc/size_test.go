package main

import "testing"

func TestParseSize(t *testing.T) {
	ok := map[string]int64{"25MiB": 25 << 20, "25M": 25 << 20, "100MB": 100e6, "10GiB": 10 << 30, "1.5G": 3 << 29,
		"4096": 4096, " 2 KiB ": 2048, "1tb": 1e12}
	for in, want := range ok {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "MB", "-1MB", "5 parsecs", "1e400", "99999999999TB"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) accepted", in)
		}
	}
}
