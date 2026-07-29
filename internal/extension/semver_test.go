package extension

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"1.2.0", "1.10.0", -1},
		{"2.0.0", "1.99.99", 1},
		{"v1.0.0", "1.0.0", 0}, // "v" prefix tolerated
	}
	for _, c := range cases {
		got, err := CompareVersions(c.a, c.b)
		if err != nil {
			t.Fatalf("CompareVersions(%q, %q): %v", c.a, c.b, err)
		}
		if got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareVersionsRejectsNonSemver(t *testing.T) {
	if _, err := CompareVersions("not-a-version", "1.0.0"); err == nil {
		t.Fatal("expected an error for a non-semver first argument")
	}
	if _, err := CompareVersions("1.0.0", "not-a-version"); err == nil {
		t.Fatal("expected an error for a non-semver second argument")
	}
}
