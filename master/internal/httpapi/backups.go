package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/ufna/birdman/master/internal/backup"
	"github.com/ufna/birdman/master/internal/store"
)

// Backups v1 (docs/superpowers/specs/2026-07-13-backups-admin-v1-design.md §4):
// settings / run history / run-now / s3-test. Every route is ScopeAdmin (see
// server.go) — the settings are secret-adjacent. The s3 secret is write-only:
// no response carries it (store.BackupSettings exposes only has_s3_secret), and
// PATCH keeps it untouched when s3_secret_key is absent (nil pointer → store
// keep) so a config edit never has to re-send the secret.

// BackupRunner is the manual run-now surface (satisfied by *backup.Runner); an
// interface so tests inject a fake without a real pg_dump.
type BackupRunner interface {
	RunNow(ctx context.Context) error
}

// patchBackupSettingsRequest is the PATCH body — every field a pointer so an
// absent field means "leave unchanged". s3_secret_key absent = keep the stored
// secret, a non-empty value rotates it, "" clears it (store validates that
// clearing is only allowed with s3_enabled=false).
type patchBackupSettingsRequest struct {
	Enabled        *bool   `json:"enabled"`
	IntervalHours  *int    `json:"interval_hours"`
	RetentionLocal *int    `json:"retention_local"`
	S3Enabled      *bool   `json:"s3_enabled"`
	S3Endpoint     *string `json:"s3_endpoint"`
	S3Region       *string `json:"s3_region"`
	S3Bucket       *string `json:"s3_bucket"`
	S3Prefix       *string `json:"s3_prefix"`
	S3AccessKey    *string `json:"s3_access_key"`
	S3SecretKey    *string `json:"s3_secret_key"`
	RetentionS3    *int    `json:"retention_s3"`
}

// handleGetBackupSettings is GET /v1/backups/settings (admin): the current
// policy, with has_s3_secret but never the secret itself.
func (s *Server) handleGetBackupSettings(w http.ResponseWriter, r *http.Request) {
	set, err := s.st.GetBackupSettings(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, backupSettingsResp{Settings: set})
}

// handlePatchBackupSettings is PATCH /v1/backups/settings (admin). The nil
// pointers carry keep-semantics straight into the store. Errors split by class:
// a validation failure (bounds, s3_enabled consistency, secret-clear rules) is
// store.ErrBadBackupSettings → 400 bad_request; anything else out of
// PatchBackupSettings is infra (tx/encrypt/commit) → storeError (default 500).
func (s *Server) handlePatchBackupSettings(w http.ResponseWriter, r *http.Request) {
	var req patchBackupSettingsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	set, err := s.st.PatchBackupSettings(r.Context(), store.BackupSettingsPatch{
		Enabled: req.Enabled, IntervalHours: req.IntervalHours,
		RetentionLocal: req.RetentionLocal, S3Enabled: req.S3Enabled,
		S3Endpoint: req.S3Endpoint, S3Region: req.S3Region, S3Bucket: req.S3Bucket,
		S3Prefix: req.S3Prefix, S3AccessKey: req.S3AccessKey,
		S3SecretKey: req.S3SecretKey, RetentionS3: req.RetentionS3,
	})
	if err != nil {
		if errors.Is(err, store.ErrBadBackupSettings) {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, backupSettingsResp{Settings: set})
}

// handleListBackupRuns is GET /v1/backups/runs?limit=N (admin): the run history
// newest-first (store clamps the limit).
func (s *Server) handleListBackupRuns(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	runs, err := s.st.ListBackupRuns(r.Context(), limit)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, backupRunsResp{Runs: emptyNotNull(runs)})
}

// handleRunBackup is POST /v1/backups/run (admin): trigger a run now. 202 once
// accepted (the runner does the dump in the background), 409 conflict when a
// run is already in progress (backup.ErrBusy), 503 when no runner is wired.
func (s *Server) handleRunBackup(w http.ResponseWriter, r *http.Request) {
	if s.backups == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "backup runner is not wired")
		return
	}
	if err := s.backups.RunNow(r.Context()); err != nil {
		if errors.Is(err, backup.ErrBusy) {
			writeError(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, backupStartedResp{Started: true})
}

// handleTestBackupS3 is POST /v1/backups/s3/test (admin): verify the saved S3
// config (bucket reachable). 200 ok, 400 s3_test_failed with the reason, 503
// when no s3-test is wired.
func (s *Server) handleTestBackupS3(w http.ResponseWriter, r *http.Request) {
	if s.backupS3Test == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "s3 test is not wired")
		return
	}
	if err := s.backupS3Test(r.Context()); err != nil {
		writeError(w, http.StatusBadRequest, "s3_test_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, okResp{OK: true})
}
