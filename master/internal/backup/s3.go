package backup

import (
	"context"
	"errors"

	"github.com/ufna/birdman/master/internal/store"
)

// syncS3 — выгрузка дампа в S3-совместимое хранилище + ротация в бакете.
// Реализация — Task 3 (minio-go); до неё включённый s3_enabled даёт
// явную ошибку прогона (дамп на диске остаётся).
func (r *Runner) syncS3(ctx context.Context, cfg store.BackupS3Config, localPath, objectName string) error {
	return errors.New("s3 sync is not built yet (Task 3)")
}
