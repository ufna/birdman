package httpapi

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
)

// The session cookie's Secure flag must follow the effective scheme: on over
// plain HTTP (the dev SSH tunnel) it must be OFF, or Safari silently drops the
// cookie and the panel login loops. Over real/proxied HTTPS it must be ON.
func TestSessionCookieSecureFollowsScheme(t *testing.T) {
	if c := sessionCookieFor("v", 60, true); !c.Secure {
		t.Error("secure=true → cookie.Secure must be set")
	}
	if c := sessionCookieFor("v", 60, false); c.Secure {
		t.Error("secure=false → cookie.Secure must be unset (dev tunnel)")
	}
}

func TestRequestIsHTTPS(t *testing.T) {
	plain := httptest.NewRequest("POST", "http://127.0.0.1:8100/v1/session", nil)
	if requestIsHTTPS(plain) {
		t.Error("plain HTTP request must be treated as non-HTTPS")
	}

	proxied := httptest.NewRequest("POST", "http://127.0.0.1:8100/v1/session", nil)
	proxied.Header.Set("X-Forwarded-Proto", "https")
	if !requestIsHTTPS(proxied) {
		t.Error("X-Forwarded-Proto=https must be treated as HTTPS")
	}

	direct := httptest.NewRequest("POST", "https://x/v1/session", nil)
	direct.TLS = &tls.ConnectionState{}
	if !requestIsHTTPS(direct) {
		t.Error("r.TLS set must be treated as HTTPS")
	}
}
