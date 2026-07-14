package store

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// w5: mapEnvFKViolation переводит 23503 на env-FK в чистый «no such environment»
// (v3: ErrBadEnv → 400 на HTTP, тот же sentinel, что и у in-tx пре-чеков) и молчит
// на всём прочем. Прямой юнит на маппер: реальную гонку (DELETE env между in-tx
// пре-чеком и insert'ом) без тестовых хуков не воспроизвести, поэтому пиним само
// отображение.
func TestMapEnvFKViolation(t *testing.T) {
	fk := func(constraint string) error {
		return &pgconn.PgError{Code: "23503", ConstraintName: constraint}
	}

	// Все env-FK (versions/fleet/api_keys/nodes) → ErrBadEnv, сообщение называет
	// окружение ровно тем текстом, что уходит в detail 400-ответа.
	for _, c := range []string{"versions_env_fk", "fleet_env_fk", "api_keys_env_fk", "nodes_env_fk"} {
		got := mapEnvFKViolation(fk(c), "game", "prod")
		if !errors.Is(got, ErrBadEnv) {
			t.Fatalf("%s: want ErrBadEnv, got %v", c, got)
		}
		if errors.Is(got, ErrNotFound) {
			t.Fatalf("%s: bad env — это 400, а не 404: %v", c, got)
		}
		if got.Error() != "no such environment game/prod" {
			t.Fatalf("%s: message must name the env, got %q", c, got.Error())
		}
	}

	// Не наши случаи → nil (вызывающий сохраняет собственную обработку).
	cases := []struct {
		name string
		err  error
	}{
		{"nil error", nil},
		{"unique violation 23505 on an env fk name", &pgconn.PgError{Code: "23505", ConstraintName: "versions_env_fk"}},
		{"a different 23503 (active_version fk)", &pgconn.PgError{Code: "23503", ConstraintName: "fleet_active_version_env_fk"}},
		{"23503 on an unrelated constraint", &pgconn.PgError{Code: "23503", ConstraintName: "servers_node_id_fkey"}},
		{"plain non-pg error", errors.New("boom")},
	}
	for _, tc := range cases {
		if got := mapEnvFKViolation(tc.err, "game", "prod"); got != nil {
			t.Fatalf("%s: want nil, got %v", tc.name, got)
		}
	}
}
