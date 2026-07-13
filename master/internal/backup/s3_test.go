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

	cli, err := newS3Client(cfg)
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	// Декои ДО ротационных загрузок: вложенный СТАРЫЙ дамп и чужой файл под
	// префиксом. Ротация работает только с прямыми детьми префикса и не
	// имеет права их трогать: вложенный birdman-*.dump с древним ts иначе
	// сортировался бы по полному ключу выше свежих прямых дампов ('k' > 'b'),
	// съедал keep-слот и ронял настоящие дампы (ревью Task 3).
	decoys := []string{
		cfg.Prefix + "keep/birdman-20200101T000000Z.dump",
		cfg.Prefix + "other.txt",
	}
	for _, k := range decoys {
		if _, err := cli.PutObject(ctx, bucket, k, strings.NewReader("decoy"),
			int64(len("decoy")), minio.PutObjectOptions{}); err != nil {
			t.Fatalf("put decoy %s: %v", k, err)
		}
	}

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

	listKeys := func() map[string]bool {
		t.Helper()
		got := map[string]bool{}
		for obj := range cli.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: cfg.Prefix, Recursive: true}) {
			if obj.Err != nil {
				t.Fatalf("list: %v", obj.Err)
			}
			got[obj.Key] = true
		}
		return got
	}

	// retention_s3=2: из прямых детей живы два свежих, старейший удалён;
	// оба декоя нетронуты.
	got := listKeys()
	want := []string{
		decoys[0],
		decoys[1],
		cfg.Prefix + "birdman-20260712T000000Z.dump",
		cfg.Prefix + "birdman-20260713T000000Z.dump",
	}
	for _, k := range want {
		if !got[k] {
			t.Fatalf("object %s missing after rotation, bucket has %v", k, got)
		}
	}
	if got[cfg.Prefix+"birdman-20260711T000000Z.dump"] {
		t.Fatalf("oldest direct dump survived rotation, bucket has %v", got)
	}
	if len(got) != len(want) {
		t.Fatalf("unexpected extra objects after rotation: %v, want exactly %v", got, want)
	}

	// Guard ретеншна: RetentionS3<1 — no-op, а не снос бакета/паника на срезе.
	cfgZero := cfg
	cfgZero.RetentionS3 = 0
	if err := rotateS3(ctx, cli, cfgZero); err != nil {
		t.Fatalf("rotateS3 with retention 0: %v", err)
	}
	if after := listKeys(); len(after) != len(want) {
		t.Fatalf("retention 0 must be a no-op, bucket has %v", after)
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
