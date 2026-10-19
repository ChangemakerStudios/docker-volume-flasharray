package config

import (
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"32GiB":      32 << 30,
		"32GB":       32 << 30,
		"32G":        32 << 30,
		"1TiB":       1 << 40,
		"512MiB":     512 << 20,
		"1.5G":       (3 << 30) / 2,
		"1073741824": 1 << 30,
		" 2 GiB ":    2 << 30,
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "abc", "-1G", "0", "100K", "1GiBB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
	// odd byte counts round up to a 512 multiple
	if got, _ := ParseSize("1048577"); got != 1048576+512 {
		t.Errorf("rounding: got %d", got)
	}
}

func TestValidateNamespace(t *testing.T) {
	for _, ok := range []string{"prod", "node-01", "A1"} {
		if err := ValidateNamespace(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "has_underscore", "has.dot", "with space", "x123456789012345678901234567890123"} {
		if err := ValidateNamespace(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestValidateLinger(t *testing.T) {
	for _, tc := range []struct {
		linger, grace time.Duration
		ok            bool
	}{
		{0, 0, true},
		{15 * time.Second, 90 * time.Second, true},
		{15 * time.Second, 0, false},                // immediate preemption would cut a lingering volume
		{30 * time.Second, 5 * time.Minute, false},  // a waiting node gives up a Mount attempt at 30s
		{20 * time.Second, 30 * time.Second, false}, // too close to the grace
		{-time.Second, 90 * time.Second, false},
	} {
		if err := ValidateLinger(tc.linger, tc.grace); (err == nil) != tc.ok {
			t.Errorf("linger %s grace %s: err = %v, want ok=%v", tc.linger, tc.grace, err, tc.ok)
		}
	}
}
