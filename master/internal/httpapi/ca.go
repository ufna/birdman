package httpapi

import (
	"bytes"
	"net/http"
)

// handleGetCA returns the active internal CA cert bundle as text/plain PEM —
// the public trust anchor ansible delivers to every node (§6) and a debugging
// aid. It concatenates store.ActiveCAs (normally one cert; two only during a
// manual CA-rotation window). Only cert PEMs are ever returned: the CA signing
// key is reachable solely through store.EnsureInternalCA, so /v1/ca has nowhere
// to read it from by construction (design §5). Readonly scope — a public cert
// is not a secret. 503 (not an empty 200) when no CA exists yet, so ansible
// never writes an empty master-ca.pem.
func (s *Server) handleGetCA(w http.ResponseWriter, r *http.Request) {
	cas, err := s.st.ActiveCAs(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	if len(cas) == 0 {
		writeError(w, http.StatusServiceUnavailable, "ca_unavailable",
			"no active internal CA on this master")
		return
	}
	var buf bytes.Buffer
	for _, pem := range cas {
		buf.Write(pem)
		// Guarantee a separating newline between concatenated PEM blocks so the
		// bundle stays parseable even if a stored cert lacks a trailing \n.
		if n := len(pem); n > 0 && pem[n-1] != '\n' {
			buf.WriteByte('\n')
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
