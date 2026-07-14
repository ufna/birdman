package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ИНВАРИАНТ: отзыв API-ключа необратим (`revoked_at` никогда не сбрасывается
// в null). На нём держится безопасность каскадного удаления окружения
// (DeleteEnvironmentCascade): привязанные ключи там сначала ОТЗЫВАЮТСЯ, а
// затем их привязка (project_id, env) снимается — иначе FK api_keys_env_fk не
// даст удалить строку environments. Утраченная пара уходит в аудит-событие
// apikey_revoked{project, env, reason}. Пока раз-отзыва не существует, ключ
// мёртв навсегда и «всплыть глобальным» не может. Если кто-то добавит
// un-revoke — этот тест упадёт и заставит сначала решить, что делать с
// осиротевшей привязкой (например, запретить раз-отзыв ключам, чьё окружение
// удалено, храня причину отзыва).
func TestRevocationIsIrreversible(t *testing.T) {
	// Скоуп — пакет store (весь SQL по api_keys живёт здесь; хендлеры ходят
	// только через его методы). Ищем именно мутацию
	// (`update … set revoked_at = null`), а не упоминание в комментарии/предикате
	// (`where revoked_at is null` — легитимный фильтр живых ключей).
	unrevoke := regexp.MustCompile(`(?is)update\s+api_keys\b[^;'`+"`"+`]*?set[^;'`+"`"+`]*?revoked_at\s*=\s*(null|default)`)
	root := "."
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if loc := unrevoke.FindIndex(src); loc != nil {
			line := 1 + strings.Count(string(src[:loc[0]]), "\n")
			t.Errorf("%s:%d: сброс revoked_at ломает инвариант необратимости отзыва "+
				"(см. DeleteEnvironmentCascade: осиротевшие ключи стали бы глобальными)", path, line)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}
