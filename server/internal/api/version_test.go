package api

import "testing"

func TestNewerVersion(t *testing.T) {
	for _, tc := range []struct {
		latest, current string
		want            bool
	}{
		{"v0.9.0", "v0.8.1", true},
		{"v0.9.0", "v0.9.0", false},
		{"v0.9.0", "v1.0.0", false},
		{"v0.9.0", "v0.9.1-rc.1", false},
		{"invalid", "v0.8.1", false},
	} {
		if got := newerVersion(tc.latest, tc.current); got != tc.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
		}
	}
}
