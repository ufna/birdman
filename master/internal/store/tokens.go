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

// ParseNodeTokenID extracts the node id embedded in a node_token
// ("bnt_<node_uuid>.<secret>") WITHOUT verifying the secret — it reuses the
// same parseToken helper AuthNodeToken uses. The confused-deputy guard on a
// cert-authenticated Session (agentlink, mTLS agentlink v1 design §3) uses it
// to require that a node_token, if the agent still sends one alongside its
// client cert, names the SAME node as the certificate's CN. Returns an error
// if the token is malformed.
func ParseNodeTokenID(token string) (string, error) {
	id, _, err := parseToken(nodeTokenPrefix, token)
	return id, err
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
