package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// testS3Cfg — скипает тест, если MinIO-обвязки нет (локальный запуск без test.sh).
func testS3Cfg(t *testing.T, bucket string) store.BackupS3Config {
	t.Helper()
	ep := os.Getenv("BIRDMAN_TEST_S3_ENDPOINT")
	if ep == "" {
		t.Skip("BIRDMAN_TEST_S3_ENDPOINT not set (run via master/test.sh)")
	}
	return store.BackupS3Config{
		Endpoint:    ep,
		Bucket:      bucket,
		Prefix:      "nightly/",
		AccessKey:   os.Getenv("BIRDMAN_TEST_S3_KEY"),
		SecretKey:   os.Getenv("BIRDMAN_TEST_S3_SECRET"),
		RetentionS3: 2,
	}
}

func mustMakeBucket(t *testing.T, cfg store.BackupS3Config) {
	t.Helper()
	cli, err := newS3Client(cfg)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if err := cli.MakeBucket(context.Background(), cfg.Bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("make bucket: %v", err)
	}
}

func TestSyncS3UploadAndRotation(t *testing.T) {
	bucket := fmt.Sprintf("bt-%d", time.Now().UnixNano())
	cfg := testS3Cfg(t, bucket)
	mustMakeBucket(t, cfg)

	dir := t.TempDir()
	r := &Runner{dir: dir, log: testLog()}
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("birdman-2026071%dT000000Z.dump", i)
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(strings.Repeat("x", 100*i)), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := r.syncS3(ctx, cfg, p, name); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}

	// retention_s3=2 → самый старый объект удалён, префикс уважен.
	cli, _ := newS3Client(cfg)
	var keys []string
	for obj := range cli.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: cfg.Prefix, Recursive: true}) {
		if obj.Err != nil {
			t.Fatalf("list: %v", obj.Err)
		}
		keys = append(keys, obj.Key)
	}
	if len(keys) != 2 {
		t.Fatalf("rotation kept %v, want 2", keys)
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, "nightly/birdman-") {
			t.Fatalf("prefix not honored: %s", k)
		}
		if strings.Contains(k, "20260711") {
			t.Fatalf("oldest object survived rotation: %s", k)
		}
	}
}

func TestTestS3(t *testing.T) {
	bucket := fmt.Sprintf("bt-%d", time.Now().UnixNano())
	cfg := testS3Cfg(t, bucket)
	st := testdb.New(t)
	ctx := context.Background()

	// Сохранить конфиг с НЕсуществующим бакетом → TestS3 ошибка.
	tr := true
	if _, err := st.PatchBackupSettings(ctx, store.BackupSettingsPatch{
		S3Enabled: &tr, S3Endpoint: &cfg.Endpoint, S3Bucket: &bucket,
		S3AccessKey: &cfg.AccessKey, S3SecretKey: &cfg.SecretKey,
	}); err != nil {
		t.Fatalf("patch: %v", err)
	}
	if err := TestS3(ctx, st); err == nil {
		t.Fatal("bucket does not exist — TestS3 must fail")
	}
	mustMakeBucket(t, cfg)
	if err := TestS3(ctx, st); err != nil {
		t.Fatalf("TestS3 after bucket creation: %v", err)
	}
}
