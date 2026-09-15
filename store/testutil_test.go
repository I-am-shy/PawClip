package store

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testLogger 只把 warn 及以上打到 stderr，避免正常用例刷屏。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func newTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(Options{Path: filepath.Join(dir, "pawclip.db"), Logger: testLogger()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db
}

func newTestBlobs(t *testing.T) *BlobStore {
	t.Helper()
	bs, err := NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatalf("NewBlobStore: %v", err)
	}
	return bs
}

func newTestWriter(t *testing.T, cfg WriterConfig) (*Writer, *DB, *BlobStore) {
	t.Helper()
	db := newTestDB(t)
	bs := newTestBlobs(t)
	w, err := NewWriter(db, bs, cfg, testLogger())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() {
		cancel()
		if err := w.Close(); err != nil {
			t.Errorf("writer Close: %v", err)
		}
	})
	return w, db, bs
}

// sampleItem 造一条可写入的条目。
func sampleItem(fp, text string) Item {
	txt := text
	return Item{
		Kind:          "text",
		TextContent:   &txt,
		Preview:       text,
		Fingerprint:   fp,
		ByteSize:      int64(len(text)),
		SourceAppID:   "com.example.app",
		SourceAppName: "Example",
		FirstSeenAt:   1000,
		CreatedAt:     1000,
	}
}

// eventually 轮询等待一个异步条件成立。
//
// 为什么需要：写入器的回执在事务 Commit 之后立刻发出，但"提交后维护"
// （计数、WAL checkpoint）在同一轮 flush 的尾部才跑。断言这些计数器时
// 必须允许一点点滞后，否则测试会随机翻绿翻红。
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时（%v）：%s", timeout, what)
}
