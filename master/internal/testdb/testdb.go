// Package testdb provisions real Postgres 16 databases for integration
// tests. Resolution order:
//
//  1. BIRDMAN_TEST_DSN env (CI service container, master/test.sh);
//  2. a postgres:16 container started via the docker CLI (host runs);
//  3. otherwise tests are skipped.
//
// Each New(t) call creates an isolated database with migrations applied and
// drops it on cleanup.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/ufna/birdman/master/internal/secrets"
	"github.com/ufna/birdman/master/internal/store"
)

var (
	adminDSN string
	skipMsg  string
)

// Run wraps testing.M — call from TestMain: os.Exit(testdb.Run(m)).
func Run(m *testing.M) int {
	var stop func()
	if dsn := os.Getenv("BIRDMAN_TEST_DSN"); dsn != "" {
		adminDSN = dsn
		// The database may still be booting (test.sh, CI service container);
		// a late failure will surface in New(t).
		_ = waitReady(dsn, 60*time.Second)
	} else if dsn, stopFn, err := startDockerPostgres(); err == nil {
		adminDSN = dsn
		stop = stopFn
	} else {
		skipMsg = fmt.Sprintf("integration tests skipped: BIRDMAN_TEST_DSN not set and docker postgres unavailable (%v)", err)
	}
	code := m.Run()
	if stop != nil {
		stop()
	}
	return code
}

func waitReady(dsn string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil {
			err = conn.Ping(ctx)
			_ = conn.Close(ctx)
		}
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	return lastErr
}

func startDockerPostgres() (string, func(), error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return "", nil, err
	}
	name := fmt.Sprintf("birdman-test-pg-%d-%d", os.Getpid(), time.Now().UnixNano()%1_000_000)
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_PASSWORD=birdman",
		"-p", "127.0.0.1:0:5432", "postgres:16").CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("docker run postgres:16: %v: %s", err, strings.TrimSpace(string(out)))
	}
	stop := func() { _ = exec.Command("docker", "rm", "-f", name).Run() }

	portOut, err := exec.Command("docker", "port", name, "5432/tcp").Output()
	if err != nil {
		stop()
		return "", nil, fmt.Errorf("docker port: %v", err)
	}
	hostPort := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(portOut)), "\n", 2)[0])
	dsn := fmt.Sprintf("postgres://postgres:birdman@%s/postgres?sslmode=disable", hostPort)

	deadline := time.Now().Add(120 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil {
			err = conn.Ping(ctx)
			_ = conn.Close(ctx)
		}
		cancel()
		if err == nil {
			return dsn, stop, nil
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	stop()
	return "", nil, fmt.Errorf("postgres not ready in time: %v", lastErr)
}

// New returns a Store bound to a fresh migrated database, encrypted at-rest
// with a RANDOM per-database key. This means the entire integration suite runs
// over encryption automatically — every UpsertRegistry/EnsureInternalCA writes
// an envelope and every read decrypts — without any test opting in.
func New(t *testing.T) *store.Store {
	t.Helper()
	st, _ := NewWithCodec(t, randomCodec(t))
	return st
}

// NewWithCodec is New with a caller-supplied codec, and also returns the
// database DSN so a test can open a SECOND store on the SAME database with a
// DIFFERENT codec — the wrong-key DR rehearsal (a restore against the wrong
// key must fail loudly, not misread). The returned DSN already carries the
// pool_max_conns tuning, so store.Open(ctx, dsn, otherCodec) reuses it.
func NewWithCodec(t *testing.T, codec *secrets.Codec) (*store.Store, string) {
	t.Helper()
	if adminDSN == "" {
		t.Skip(skipMsg)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	raw := make([]byte, 6)
	_, _ = rand.Read(raw)
	name := "bt_" + hex.EncodeToString(raw)
	if _, err := admin.Exec(ctx, "create database "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}

	if err := store.MigrateUp(withDatabase(t, adminDSN, name)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dsn := withParam(t, withDatabase(t, adminDSN, name), "pool_max_conns", "20")
	// Рабочий фактор bcrypt на минимум — ЕДИНСТВЕННОЕ место во всём дереве,
	// которое его понижает, и понижает ровно у тестовых сторов (tracker #1060).
	// При bcrypt.DefaultCost один хеш под `-race` стоит ~0.7с, а сьют заводит
	// сотни ключей и нод — это было около 40% времени пакета httpapi. Проверки
	// от этого не слабеют: путь «хеш → БД → сверка» проходится целиком (сверка
	// читает стоимость из самого хеша), отказ на неверном секрете остаётся
	// отказом, ни один тест ничего не утверждает о стоимости. Прод-дефолт
	// (bcrypt.DefaultCost) закреплён тестами в пакете store — см. hashcost.go.
	st, err := store.Open(ctx, dsn, codec, store.WithHashCostForTests(bcrypt.MinCost))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
		if _, err := admin.Exec(ctx, "drop database "+name+" with (force)"); err != nil {
			t.Logf("drop database %s: %v", name, err)
		}
		_ = admin.Close(ctx)
	})
	return st, dsn
}

// randomCodec builds a codec from 32 random bytes — a throwaway per-database
// key. Tests that need a specific/second key use NewWithCodec instead.
func randomCodec(t *testing.T) *secrets.Codec {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("random key: %v", err)
	}
	c, err := secrets.New(key)
	if err != nil {
		t.Fatalf("new codec: %v", err)
	}
	return c
}

func withDatabase(t *testing.T, dsn, db string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + db
	return u.String()
}

func withParam(t *testing.T, dsn, key, value string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}
