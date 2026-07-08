package matchmaker

import (
	"fmt"
	"strconv"
	"strings"
)

// MajorMinor reduces a semver-ish version ("1.4.2", "v1.4", "1.4.2-rc1") to
// its "MAJOR.MINOR" compat bucket. Client and server are compatible when the
// buckets are equal — the default rule of docs/specs/ops.md §3. Migration
// windows extend it via compat.overrides (Compat below, итерация 3).
func MajorMinor(v string) (string, error) {
	sv, err := parseSemver(v)
	if err != nil {
		return "", err
	}
	return strconv.Itoa(sv.major) + "." + strconv.Itoa(sv.minor), nil
}

// semver is a parsed MAJOR.MINOR[.PATCH] version; pre-release/build suffixes
// are ignored for compatibility purposes (уточнено в v0, ops.md §3).
type semver struct {
	major, minor, patch int
}

func parseSemver(v string) (semver, error) {
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".", 3)
	if len(parts) < 2 {
		return semver{}, fmt.Errorf("version %q: want MAJOR.MINOR[.PATCH]", v)
	}
	num := func(raw, what string) (int, error) {
		// Tolerate pre-release/build suffixes ("1.4-rc1", "1.4.2+build5").
		raw, _, _ = strings.Cut(raw, "-")
		raw, _, _ = strings.Cut(raw, "+")
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("version %q: bad %s", v, what)
		}
		return n, nil
	}
	var sv semver
	var err error
	if sv.major, err = num(parts[0], "major"); err != nil {
		return semver{}, err
	}
	if sv.minor, err = num(parts[1], "minor"); err != nil {
		return semver{}, err
	}
	if len(parts) == 3 {
		if sv.patch, err = num(parts[2], "patch"); err != nil {
			return semver{}, err
		}
	}
	return sv, nil
}

// pattern matches versions for compat.overrides (ops.md §3): literal
// components with `x`/`X`/`*` wildcards; missing trailing components are
// wildcards too. "1.4.x" ≡ "1.4" (any 1.4 patch), "1.x" ≡ "1" (any 1.*),
// "1.4.2" — exactly that patch.
type pattern struct {
	major, minor, patch int
	anyMinor, anyPatch  bool
}

func parsePattern(raw string) (pattern, error) {
	p := pattern{anyMinor: true, anyPatch: true}
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(raw), "v"), ".")
	if len(parts) == 0 || len(parts) > 3 || parts[0] == "" {
		return pattern{}, fmt.Errorf("compat pattern %q: want MAJOR[.MINOR[.PATCH]]", raw)
	}
	wild := func(s string) bool { return s == "x" || s == "X" || s == "*" }
	if wild(parts[0]) {
		return pattern{}, fmt.Errorf("compat pattern %q: major must be a number", raw)
	}
	var err error
	if p.major, err = strconv.Atoi(parts[0]); err != nil || p.major < 0 {
		return pattern{}, fmt.Errorf("compat pattern %q: bad major", raw)
	}
	if len(parts) >= 2 && !wild(parts[1]) {
		if p.minor, err = strconv.Atoi(parts[1]); err != nil || p.minor < 0 {
			return pattern{}, fmt.Errorf("compat pattern %q: bad minor", raw)
		}
		p.anyMinor = false
	}
	if len(parts) == 3 && !wild(parts[2]) {
		if p.anyMinor {
			return pattern{}, fmt.Errorf("compat pattern %q: literal patch under wildcard minor", raw)
		}
		if p.patch, err = strconv.Atoi(parts[2]); err != nil || p.patch < 0 {
			return pattern{}, fmt.Errorf("compat pattern %q: bad patch", raw)
		}
		p.anyPatch = false
	}
	return p, nil
}

func (p pattern) matches(sv semver) bool {
	if sv.major != p.major {
		return false
	}
	if !p.anyMinor && sv.minor != p.minor {
		return false
	}
	if !p.anyPatch && sv.patch != p.patch {
		return false
	}
	return true
}

// Override is one compat.overrides row from the master config (ops.md §3):
// clients matching Client may ALSO play on servers matching any of Servers
// (additive to the default MAJOR.MINOR rule — the migration-window
// semantics).
type Override struct {
	Client  string
	Servers []string
}

// Compat decides client↔server version compatibility: the default
// MAJOR.MINOR rule extended by config overrides. The zero value (no
// overrides) implements the plain default rule.
type Compat struct {
	overrides []compiledOverride
}

type compiledOverride struct {
	client  pattern
	servers []pattern
}

// NewCompat validates and compiles the overrides table.
func NewCompat(overrides []Override) (*Compat, error) {
	c := &Compat{}
	for i, o := range overrides {
		cp, err := parsePattern(o.Client)
		if err != nil {
			return nil, fmt.Errorf("compat.overrides[%d].client: %w", i, err)
		}
		if len(o.Servers) == 0 {
			return nil, fmt.Errorf("compat.overrides[%d]: servers list is empty", i)
		}
		co := compiledOverride{client: cp}
		for j, s := range o.Servers {
			sp, err := parsePattern(s)
			if err != nil {
				return nil, fmt.Errorf("compat.overrides[%d].servers[%d]: %w", i, j, err)
			}
			co.servers = append(co.servers, sp)
		}
		c.overrides = append(c.overrides, co)
	}
	return c, nil
}

func (c *Compat) clientOverrides(sv semver) []int {
	if c == nil {
		return nil
	}
	var idx []int
	for i, o := range c.overrides {
		if o.client.matches(sv) {
			idx = append(idx, i)
		}
	}
	return idx
}

// BucketOf returns the queue bucket for a client version: MAJOR.MINOR plus
// the set of overrides the version matches. Clients share a bucket iff their
// compatibility set is identical — a group formed inside one bucket always
// fits one server.
func (c *Compat) BucketOf(clientVersion string) (string, error) {
	sv, err := parseSemver(clientVersion)
	if err != nil {
		return "", err
	}
	bucket := strconv.Itoa(sv.major) + "." + strconv.Itoa(sv.minor)
	for _, i := range c.clientOverrides(sv) {
		bucket += fmt.Sprintf("+o%d", i)
	}
	return bucket, nil
}

// Compatible reports whether a client may play on a server version: equal
// MAJOR.MINOR (default rule) OR any matching override allows the server.
// Unparseable server semvers are incompatible (client versions are validated
// at ticket submit).
func (c *Compat) Compatible(clientVersion, serverSemver string) bool {
	cv, err := parseSemver(clientVersion)
	if err != nil {
		return false
	}
	sv, err := parseSemver(serverSemver)
	if err != nil {
		return false
	}
	if cv.major == sv.major && cv.minor == sv.minor {
		return true
	}
	for _, i := range c.clientOverrides(cv) {
		for _, sp := range c.overrides[i].servers {
			if sp.matches(sv) {
				return true
			}
		}
	}
	return false
}
