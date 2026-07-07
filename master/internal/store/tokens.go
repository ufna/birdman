package store

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Bearer secrets are "<prefix><uuid>.<secret>": the uuid gives O(1) row
// lookup, only bcrypt(secret) is stored (docs/specs/master.md §6).
const (
	nodeTokenPrefix = "bnt_"
	apiKeyPrefix    = "bmk_"
)

func newSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func composeToken(prefix, id, secret string) string {
	return prefix + id + "." + secret
}

func parseToken(prefix, token string) (id, secret string, err error) {
	rest, ok := strings.CutPrefix(token, prefix)
	if !ok {
		return "", "", fmt.Errorf("bad token prefix")
	}
	id, secret, ok = strings.Cut(rest, ".")
	if !ok || secret == "" {
		return "", "", fmt.Errorf("bad token format")
	}
	if _, err := uuid.Parse(id); err != nil {
		return "", "", fmt.Errorf("bad token id")
	}
	return id, secret, nil
}
