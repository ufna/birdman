package matchmaker

import (
	"fmt"
	"strconv"
	"strings"
)

// MajorMinor reduces a semver-ish version ("1.4.2", "v1.4", "1.4.2-rc1") to
// its "MAJOR.MINOR" compat bucket. Client and server are compatible when the
// buckets are equal — the default rule of docs/specs/ops.md §3. The
// per-project compat overrides table (migration windows) is a later
// iteration; v0 implements the default rule only (уточнено в v0).
func MajorMinor(v string) (string, error) {
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".", 3)
	if len(parts) < 2 {
		return "", fmt.Errorf("version %q: want MAJOR.MINOR[.PATCH]", v)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil || major < 0 {
		return "", fmt.Errorf("version %q: bad major", v)
	}
	// Tolerate pre-release/build suffixes on the minor ("1.4-rc1").
	minorStr, _, _ := strings.Cut(parts[1], "-")
	minorStr, _, _ = strings.Cut(minorStr, "+")
	minor, err := strconv.Atoi(minorStr)
	if err != nil || minor < 0 {
		return "", fmt.Errorf("version %q: bad minor", v)
	}
	return strconv.Itoa(major) + "." + strconv.Itoa(minor), nil
}
