package config

import "testing"

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
