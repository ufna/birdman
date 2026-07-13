package backup

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/ufna/birdman/master/internal/store"
)

// S3-выгрузка Backups v1 (спека §2): generic S3-compatible (B2/Wasabi/AWS/
// MinIO) через minio-go. Endpoint храним полным http(s)-URL, minio-go
// принимает host — парсим на месте. Секрет в ошибки не попадает (minio-go
// в тексты ошибок креды не кладёт; наши обёртки — только endpoint/bucket).

func newS3Client(cfg store.BackupS3Config) (*minio.Client, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bad s3 endpoint %q", cfg.Endpoint)
	}
	return minio.New(u.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: u.Scheme == "https",
		Region: cfg.Region,
	})
}

// syncS3 — положить свежий дамп и отротировать бакет до RetentionS3 штук.
func (r *Runner) syncS3(ctx context.Context, cfg store.BackupS3Config, localPath, objectName string) error {
	cli, err := newS3Client(cfg)
	if err != nil {
		return err
	}
	key := cfg.Prefix + objectName
	if _, err := cli.FPutObject(ctx, cfg.Bucket, key, localPath,
		minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return rotateS3(ctx, cli, cfg)
}

// rotateS3 — держим RetentionS3 свежих birdman-*.dump среди ПРЯМЫХ ДЕТЕЙ
// префикса. Вложенные ключи (prefix+"keep/…", prefix+"archive/…") — не наши:
// syncS3 пишет только prefix+имя, а инвариант «ts в имени лексикографичен ⇒
// сортировка по ключу = сортировка по времени» верен только для прямых
// детей. Вложенный birdman-*.dump с древним ts иначе сортировался бы по
// полному ключу выше свежих дампов, съедал keep-слот и ронял настоящие
// дампы (при RetentionS3=1 — включая только что загруженный).
func rotateS3(ctx context.Context, cli *minio.Client, cfg store.BackupS3Config) error {
	if cfg.RetentionS3 < 1 {
		// Защита в глубину: нулевой/отрицательный ретеншн не имеет права
		// снести бакет (или паниковать на срезе), что бы ни пропустил store.
		return nil
	}
	var keys []string
	for obj := range cli.ListObjects(ctx, cfg.Bucket, minio.ListObjectsOptions{
		Prefix: cfg.Prefix, Recursive: true,
	}) {
		if obj.Err != nil {
			return fmt.Errorf("list %s: %w", cfg.Bucket, obj.Err)
		}
		rest := strings.TrimPrefix(obj.Key, cfg.Prefix)
		if strings.Contains(rest, "/") {
			continue // вложенный ключ — не наш: syncS3 пишет только прямых детей
		}
		if strings.HasPrefix(rest, "birdman-") && strings.HasSuffix(rest, ".dump") {
			keys = append(keys, obj.Key)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	for _, old := range keys[min(cfg.RetentionS3, len(keys)):] {
		if err := cli.RemoveObject(ctx, cfg.Bucket, old, minio.RemoveObjectOptions{}); err != nil {
			return fmt.Errorf("remove %s: %w", old, err)
		}
	}
	return nil
}

// TestS3 — «Проверить соединение» из панели: по СОХРАНЁННОЙ конфигурации
// (strict-decrypt секрета) убедиться, что бакет существует и доступен.
func TestS3(ctx context.Context, st *store.Store) error {
	cfg, err := st.BackupS3Config(ctx)
	if err != nil {
		return err
	}
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return errors.New("s3 endpoint and bucket are not configured")
	}
	cli, err := newS3Client(cfg)
	if err != nil {
		return err
	}
	ok, err := cli.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return fmt.Errorf("s3 check failed: %w", err)
	}
	if !ok {
		return fmt.Errorf("bucket %q does not exist or is not accessible", cfg.Bucket)
	}
	return nil
}
