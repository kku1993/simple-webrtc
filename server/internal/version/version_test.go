package version

import "testing"

func TestMajorFromString(t *testing.T) {
	cases := map[string]int{
		"0.8.1":     0,
		"1.0.0":     1,
		"2.3.4":     2,
		"10.0.0":    10,
		"dev":       -1,
		"":          -1,
		"abc":       -1,
		"v1.0.0":    -1, // no leading 'v' — the VERSION file is bare semver
		"1":         1,  // major-only is tolerated
		"1.2":       1,  // major.minor is tolerated
		"-1.0.0":    -1, // negative major is not valid
	}
	for in, want := range cases {
		if got := MajorFromString(in); got != want {
			t.Errorf("MajorFromString(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestMajorDefaultIsDev(t *testing.T) {
	// Without ldflags the package var is "dev"; Major() must return -1 so the
	// server skips the protocol-version check in development/test builds.
	if Major() != -1 {
		t.Errorf("Major() = %d, want -1 for default \"dev\"", Major())
	}
}
