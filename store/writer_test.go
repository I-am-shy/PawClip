package store

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"testing"
	"time"
)

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 64, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestWriter_FlushCommitsAndReplies(t *testing.T) {
	w, db, _ := newTestWriter(t, WriterConfig{})
	ctx := context.Background()

	ack := make(chan WriteResult, 1)
	if err := w.Enqueue(WriteRequest{Item: sampleItem("sha256:w1", "内容一"), Result: ack}); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-ack:
		if res.Err != nil {
			t.Fatalf("写入失败: %v", res.Err)
		}
		if res.ID == 0 || res.UseCount != 1 {
			t.Fatalf("结果 = %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等回执超时")
	}

	n, err := db.CountAlive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("条目数 = %d, want 1", n)
	}
}

// HANDOFF-PROMPT 验收 2：同一内容复制 100 次 → 1 行且 use_count = 100。
// 这里走完整写入 goroutine + 合批路径。
func TestWriter_DedupeHundredThroughBatches(t *testing.T) {
	w, db, _ := newTestWriter(t, WriterConfig{})
	ctx := context.Background()

	const n = 100
	acks := make([]chan WriteResult, 0, n)
	for i := 0; i < n; i++ {
		ack := make(chan WriteResult, 1)
		acks = append(acks, ack)
		it := sampleItem("sha256:same", "同一内容")
		it.CreatedAt = int64(1000 + i)
		it.LastUsedAt = ptrInt64(int64(1000 + i))
		if err := w.Enqueue(WriteRequest{Item: it, Result: ack}); err != nil {
			t.Fatalf("第 %d 次 Enqueue: %v", i+1, err)
		}
	}
	for i, ack := range acks {
		select {
		case res := <-ack:
			if res.Err != nil {
				t.Fatalf("第 %d 条写入失败: %v", i+1, res.Err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("第 %d 条等回执超时", i+1)
		}
	}

	alive, _ := db.CountAlive(ctx)
	if alive != 1 {
		t.Fatalf("存活条目数 = %d, want 1", alive)
	}
	got, err := db.GetByFingerprint(ctx, "sha256:same")
	if err != nil {
		t.Fatal(err)
	}
	if got.UseCount != n {
		t.Fatalf("use_count = %d, want %d", got.UseCount, n)
	}

	// 合批应该真的生效：100 条不该产生 100 个事务
	st := w.Stats()
	if st.Batches >= n {
		t.Errorf("批次数 = %d（100 条几乎一条一批，合批没生效）", st.Batches)
	}
	t.Logf("100 条写入 → %d 批，最后一批耗时 %dms", st.Batches, st.LastFlushMs)
}

func TestWriter_BatchMaxTriggersImmediateFlush(t *testing.T) {
	w, db, _ := newTestWriter(t, WriterConfig{
		BatchMax:      5,
		FlushInterval: time.Hour, // 故意设得很长：只能靠 BatchMax 触发
	})
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		it := sampleItem("sha256:batch-"+string(rune('a'+i)), "内容")
		if err := w.Enqueue(WriteRequest{Item: it}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := db.CountAlive(ctx); n == 5 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	n, _ := db.CountAlive(ctx)
	t.Fatalf("攒满 BatchMax 应立即提交，实际只落库 %d 条", n)
}

func TestWriter_WriteBlobAndThumbnail(t *testing.T) {
	w, db, bs := newTestWriter(t, WriterConfig{})
	ctx := context.Background()
	pngBytes := makePNG(t, 400, 200)

	it := Item{
		Kind:        "image",
		Preview:     "图片 400 × 200",
		Fingerprint: "sha256:img",
		SourceAppID: "com.example.app",
		FirstSeenAt: 1,
		CreatedAt:   1,
	}
	ack := make(chan WriteResult, 1)
	if err := w.Enqueue(WriteRequest{
		Item:   it,
		Blobs:  []BlobWrite{{Role: BlobRoleImage, Data: pngBytes, Ext: "png", Thumb: true}},
		Result: ack,
	}); err != nil {
		t.Fatal(err)
	}
	res := <-ack
	if res.Err != nil {
		t.Fatalf("写入失败: %v", res.Err)
	}

	got, err := db.GetByFingerprint(ctx, "sha256:img")
	if err != nil {
		t.Fatal(err)
	}
	if got.ImagePath == nil {
		t.Fatal("image_path 未回填")
	}
	if got.ThumbPath == nil {
		t.Fatal("thumb_path 未回填")
	}
	if !bs.Exists(*got.ImagePath) {
		t.Fatalf("图片文件不存在：%s", *got.ImagePath)
	}
	if !bs.Exists(*got.ThumbPath) {
		t.Fatalf("缩略图文件不存在：%s", *got.ThumbPath)
	}
	if got.ByteSize != int64(len(pngBytes)) {
		t.Fatalf("byte_size = %d, want %d（blob 字节数应被累加）", got.ByteSize, len(pngBytes))
	}

	thumb, err := bs.Get(*got.ThumbPath)
	if err != nil {
		t.Fatal(err)
	}
	tw, th, err := DecodePNGSize(thumb)
	if err != nil {
		t.Fatal(err)
	}
	if tw != ThumbMaxEdge || th != ThumbMaxEdge/2 {
		t.Fatalf("缩略图尺寸 = %dx%d, want 160x80", tw, th)
	}
}

func TestWriter_BlobWriteIsAtomic(t *testing.T) {
	// 先落 blob 再插行：崩在中间只会留孤儿 blob，不会留指向空文件的悬空行。
	// 这里验证"blob 一定先于行存在"。
	w, db, bs := newTestWriter(t, WriterConfig{})
	ctx := context.Background()
	pngBytes := makePNG(t, 8, 8)

	ack := make(chan WriteResult, 1)
	_ = w.Enqueue(WriteRequest{
		Item:   sampleItem("sha256:order", "x"),
		Blobs:  []BlobWrite{{Role: BlobRoleImage, Data: pngBytes, Ext: "png"}},
		Result: ack,
	})
	res := <-ack
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	got, _ := db.GetByFingerprint(ctx, "sha256:order")
	if got.ImagePath == nil || !bs.Exists(*got.ImagePath) {
		t.Fatal("行已入库但 blob 不存在（顺序反了）")
	}
}

func TestWriter_CloseDrainsQueue(t *testing.T) {
	db := newTestDB(t)
	bs := newTestBlobs(t)
	w, err := NewWriter(db, bs, WriterConfig{FlushInterval: time.Hour}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	w.Start(context.Background())

	const n = 30
	for i := 0; i < n; i++ {
		it := sampleItem("sha256:drain-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "内容")
		if err := w.Enqueue(WriteRequest{Item: it}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := db.CountAlive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != n {
		t.Fatalf("Close 后应把队列排空，落库 %d 条, want %d", got, n)
	}
	// 关掉之后再写必须被明确拒绝，而不是 panic
	if err := w.Enqueue(WriteRequest{Item: sampleItem("sha256:after", "x")}); err != ErrWriterClosed {
		t.Fatalf("err = %v, want ErrWriterClosed", err)
	}
}

func TestWriter_QueueFullIsReportedNotBlocking(t *testing.T) {
	// 不启动 goroutine，队列必然填满；Enqueue 必须在超时后返回 ErrQueueFull
	// 而不是无限阻塞（捕获 goroutine 卡死会让整条监听链路停摆）。
	db := newTestDB(t)
	bs := newTestBlobs(t)
	w, err := NewWriter(db, bs, WriterConfig{
		QueueSize:      2,
		EnqueueTimeout: 30 * time.Millisecond,
	}, testLogger())
	if err != nil {
		t.Fatal(err)
	}

	if err := w.Enqueue(WriteRequest{Item: sampleItem("sha256:a", "x")}); err != nil {
		t.Fatal(err)
	}
	if err := w.Enqueue(WriteRequest{Item: sampleItem("sha256:b", "x")}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = w.Enqueue(WriteRequest{Item: sampleItem("sha256:c", "x")})
	elapsed := time.Since(start)
	if err != ErrQueueFull {
		t.Fatalf("err = %v, want ErrQueueFull", err)
	}
	if elapsed < 25*time.Millisecond {
		t.Fatalf("应至少等到超时才放弃，实际只等了 %v", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("等待过久：%v", elapsed)
	}
	if st := w.Stats(); st.Rejected != 1 {
		t.Fatalf("Rejected = %d, want 1", st.Rejected)
	}
}

func TestWriter_CheckpointEveryN(t *testing.T) {
	w, db, _ := newTestWriter(t, WriterConfig{CheckpointEvery: 3})
	ctx := context.Background()

	for i := 0; i < 9; i++ {
		it := sampleItem("sha256:ck-"+string(rune('a'+i)), "内容")
		if err := w.Enqueue(WriteRequest{Item: it}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// checkpoint 是"提交后维护"，回执发出后才跑，所以要允许一点滞后
	eventually(t, 2*time.Second, "达到写入阈值后应做过 WAL checkpoint", func() bool {
		return w.Stats().Checkpoints > 0
	})
	if st := w.Stats(); st.Items < 9 {
		t.Fatalf("Items = %d, want >= 9", st.Items)
	}
	if n, _ := db.CountAlive(ctx); n != 9 {
		t.Fatalf("落库 %d 条, want 9", n)
	}
}

func TestWriter_BadRequestDoesNotBreakBatch(t *testing.T) {
	// 一条坏数据（缺 kind）不能拖垮同一批里的其它条目
	w, db, _ := newTestWriter(t, WriterConfig{BatchMax: 3, FlushInterval: 30 * time.Millisecond})
	ctx := context.Background()

	bad := Item{Fingerprint: "sha256:bad", FirstSeenAt: 1, CreatedAt: 1} // 无 kind
	good := sampleItem("sha256:good", "好数据")

	badAck := make(chan WriteResult, 1)
	goodAck := make(chan WriteResult, 1)
	if err := w.Enqueue(WriteRequest{Item: bad, Result: badAck}); err != nil {
		t.Fatal(err)
	}
	if err := w.Enqueue(WriteRequest{Item: good, Result: goodAck}); err != nil {
		t.Fatal(err)
	}

	if res := <-badAck; res.Err == nil {
		t.Fatal("坏条目应返回错误")
	}
	if res := <-goodAck; res.Err != nil {
		t.Fatalf("好条目不该被带坏: %v", res.Err)
	}
	if n, _ := db.CountAlive(ctx); n != 1 {
		t.Fatalf("落库 %d 条, want 1", n)
	}
	if st := w.Stats(); st.Errors == 0 {
		t.Fatal("失败次数应被统计")
	}
}

func TestWriter_ContextCancelDrainsRemaining(t *testing.T) {
	db := newTestDB(t)
	bs := newTestBlobs(t)
	w, err := NewWriter(db, bs, WriterConfig{FlushInterval: time.Hour}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)

	for i := 0; i < 10; i++ {
		it := sampleItem("sha256:cancel-"+string(rune('a'+i)), "内容")
		if err := w.Enqueue(WriteRequest{Item: it}); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	n, err := db.CountAlive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Fatalf("ctx 取消后应把剩余批次刷完，落库 %d 条, want 10", n)
	}
}

func TestWriter_ConfigDefaults(t *testing.T) {
	c := WriterConfig{}.withDefaults()
	if c.FlushInterval != 50*time.Millisecond {
		t.Errorf("FlushInterval = %v, DESIGN.md §14 要求 50ms", c.FlushInterval)
	}
	if c.BatchMax != 20 {
		t.Errorf("BatchMax = %d, DESIGN.md §14 要求 20 条", c.BatchMax)
	}
	if c.QueueSize != 512 {
		t.Errorf("QueueSize = %d", c.QueueSize)
	}
	if c.CheckpointInterval != time.Hour {
		t.Errorf("CheckpointInterval = %v", c.CheckpointInterval)
	}
}

func TestNewWriter_Validations(t *testing.T) {
	db := newTestDB(t)
	if _, err := NewWriter(nil, newTestBlobs(t), WriterConfig{}, nil); err == nil {
		t.Fatal("缺 db 应报错")
	}
	if _, err := NewWriter(db, nil, WriterConfig{}, nil); err == nil {
		t.Fatal("缺 blob store 应报错")
	}
}
