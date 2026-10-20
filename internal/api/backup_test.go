package api

import "testing"

func TestBackupScopeCompatibility(t *testing.T) {
	for _, tc := range []struct {
		a, at, p, pt string
		want         bool
	}{
		{"global", "", "global", "", true}, {"global", "", "user", "", false},
		{"team", "engineering", "team", "engineering", true}, {"team", "engineering", "team", "other", false},
		{"team", "engineering", "user", "", false}, {"team", "engineering", "global", "", true},
		{"user", "", "team", "engineering", true},
	} {
		if got := backupScopeCompatible(tc.a, tc.at, tc.p, tc.pt); got != tc.want {
			t.Errorf("%+v got %v", tc, got)
		}
	}
}
