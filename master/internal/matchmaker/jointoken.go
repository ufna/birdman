package matchmaker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Join token (docs/specs/master.md §4): HMAC-signed proof that a player got
// this match from the matchmaker — the dedicated server rejects direct
// host:port walk-ins. Off by default (config matchmaking.join_token.enabled).
//
// Format: base64url(match_id|player_id|exp_unix) + "." + base64url(hmac).
// Verification on the dedicated server goes through liba/agent
// (protocol.md §2 verify_token) — TODO liba, later iteration; VerifyJoinToken
// below is the reference implementation for that path and for tests.

func joinTokenMAC(secret []byte, payload string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

// GenerateJoinToken signs (match_id, player_id) until exp.
func GenerateJoinToken(secret []byte, matchID, playerID string, exp time.Time) string {
	payload := matchID + "|" + playerID + "|" + strconv.FormatInt(exp.Unix(), 10)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(joinTokenMAC(secret, payload))
}

// VerifyJoinToken checks the signature and expiry and returns the claims.
func VerifyJoinToken(secret []byte, token string, now time.Time) (matchID, playerID string, err error) {
	payloadB64, sigB64, ok := strings.Cut(token, ".")
	if !ok {
		return "", "", fmt.Errorf("join token: bad format")
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return "", "", fmt.Errorf("join token: bad payload encoding")
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return "", "", fmt.Errorf("join token: bad signature encoding")
	}
	payload := string(payloadRaw)
	if !hmac.Equal(sig, joinTokenMAC(secret, payload)) {
		return "", "", fmt.Errorf("join token: bad signature")
	}
	parts := strings.Split(payload, "|")
	if len(parts) != 3 {
		return "", "", fmt.Errorf("join token: bad payload")
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", "", fmt.Errorf("join token: bad expiry")
	}
	if now.Unix() > exp {
		return "", "", fmt.Errorf("join token: expired")
	}
	return parts[0], parts[1], nil
}
