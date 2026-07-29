package extension

import (
	"fmt"
	"strconv"
	"strings"
)

// This is a deliberately small semver implementation: enough to check a
// plugin's Manifest.CoreAPI constraint against the host CoreAPIVersion, without
// pulling in a dependency. It supports exact ("0.1.0") and caret ("^0.1.0")
// constraints — the two forms the SDK doc uses. Pre-release / build metadata is
// not modeled; core_api versions are plain MAJOR.MINOR.PATCH.

type semver struct{ major, minor, patch int }

func parseSemver(s string) (semver, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	// drop any pre-release/build suffix, we compare release numbers only.
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("not a MAJOR.MINOR.PATCH version: %q", s)
	}
	var v semver
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, fmt.Errorf("invalid version component %q in %q", p, s)
		}
		switch i {
		case 0:
			v.major = n
		case 1:
			v.minor = n
		case 2:
			v.patch = n
		}
	}
	return v, nil
}

// less reports v < o.
func (v semver) less(o semver) bool {
	if v.major != o.major {
		return v.major < o.major
	}
	if v.minor != o.minor {
		return v.minor < o.minor
	}
	return v.patch < o.patch
}

// caretUpper returns the exclusive upper bound of a caret range for v, following
// npm/cargo semantics: the left-most non-zero component is the compatibility
// boundary. ^1.2.3 -> <2.0.0; ^0.1.2 -> <0.2.0; ^0.0.3 -> <0.0.4.
func caretUpper(v semver) semver {
	switch {
	case v.major > 0:
		return semver{v.major + 1, 0, 0}
	case v.minor > 0:
		return semver{0, v.minor + 1, 0}
	default:
		return semver{0, 0, v.patch + 1}
	}
}

// CompareVersions compares two MAJOR.MINOR.PATCH version strings, returning
// -1 if a<b, 0 if a==b, 1 if a>b. Exported so other packages that need a
// monotonic-version check (e.g. the theme marketplace's anti-rollback guard,
// internal/thememarket) can reuse this minimal comparator instead of either
// pulling in a full semver dependency or falling back to a naive string
// inequality (which can't tell "upgrade" from "downgrade", only "different").
// Returns an error if either string isn't a plain MAJOR.MINOR.PATCH — callers
// should treat that as "not comparable" and fall back to their own policy
// rather than erroring outright, since not every version string a third
// party supplies is strict semver.
func CompareVersions(a, b string) (int, error) {
	av, err := parseSemver(a)
	if err != nil {
		return 0, err
	}
	bv, err := parseSemver(b)
	if err != nil {
		return 0, err
	}
	switch {
	case av.less(bv):
		return -1, nil
	case bv.less(av):
		return 1, nil
	default:
		return 0, nil
	}
}

// satisfies reports whether the concrete version `have` meets the semver
// `constraint`. Empty constraint means "any". Supported forms: "^X.Y.Z" (caret)
// and "X.Y.Z" (exact).
func satisfies(have, constraint string) (bool, error) {
	constraint = strings.TrimSpace(constraint)
	if constraint == "" || constraint == "*" {
		return true, nil
	}
	hv, err := parseSemver(have)
	if err != nil {
		return false, fmt.Errorf("host version: %w", err)
	}
	if strings.HasPrefix(constraint, "^") {
		base, err := parseSemver(constraint[1:])
		if err != nil {
			return false, fmt.Errorf("constraint %q: %w", constraint, err)
		}
		upper := caretUpper(base)
		// base <= hv < upper
		return !hv.less(base) && hv.less(upper), nil
	}
	// exact
	cv, err := parseSemver(constraint)
	if err != nil {
		return false, fmt.Errorf("constraint %q: %w", constraint, err)
	}
	return hv == cv, nil
}
