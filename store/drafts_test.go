package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// ── 目录与排序 ──────────────────────────────────────────────────

// TestDrafts_CreateListReorder 覆盖目录的三条核心行为：
// 默认按创建顺序、拖拽后按拖出来的顺序、归档不动 sort_order。
func TestDrafts_CreateListReorder(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	ids := make([]int64, 0, 3)
	for i, title := range []string{"草稿1", "草稿2", "草稿3"} {
		id, err := db.CreateDraft(ctx, title, int64(i+1))
		if err != nil {
			t.Fatalf("CreateDraft(%s): %v", title, err)
		}
		ids = append(ids, id)
	}
	// 正文各不相同，才能验证列表读的是摘要而不是别的列。
	for i, id := range ids {
		if _, err := db.SaveDraftMD(ctx, id, strings.Repeat("x", i+1)); err != nil {
			t.Fatalf("SaveDraftMD(%d): %v", id, err)
		}
	}

	got := titles(t, db)
	want := []string{"草稿1", "草稿2", "草稿3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("默认顺序 = %v，期望 %v（应当是创建顺序）", got, want)
	}

	// 拖成 3 → 1 → 2。
	if err := db.ReorderDrafts(ctx, []int64{ids[2], ids[0], ids[1]}); err != nil {
		t.Fatalf("ReorderDrafts: %v", err)
	}
	got = titles(t, db)
	want = []string{"草稿3", "草稿1", "草稿2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("拖拽后顺序 = %v，期望 %v", got, want)
	}

	// 归档中间那条，剩下的相对顺序必须不变。
	if err := db.ArchiveDraft(ctx, ids[0]); err != nil {
		t.Fatalf("ArchiveDraft: %v", err)
	}
	got = titles(t, db)
	want = []string{"草稿3", "草稿2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("归档后顺序 = %v，期望 %v", got, want)
	}

	arch := archivedTitles(t, db)
	if !reflect.DeepEqual(arch, []string{"草稿1"}) {
		t.Fatalf("归档区 = %v，期望 [草稿1]", arch)
	}

	// 恢复必须回到原位，而不是跳到末尾：归档没有动 sort_order。
	if err := db.RestoreDraft(ctx, ids[0]); err != nil {
		t.Fatalf("RestoreDraft: %v", err)
	}
	got = titles(t, db)
	want = []string{"草稿3", "草稿1", "草稿2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("恢复后顺序 = %v，期望 %v（恢复应当回到原位）", got, want)
	}
}

func titles(t *testing.T, db *DB) []string {
	t.Helper()
	rows, err := db.ListDrafts(context.Background())
	if err != nil {
		t.Fatalf("ListDrafts: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Title)
	}
	return out
}

func archivedTitles(t *testing.T, db *DB) []string {
	t.Helper()
	rows, err := db.ListArchivedDrafts(context.Background())
	if err != nil {
		t.Fatalf("ListArchivedDrafts: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Title)
	}
	return out
}

// ── 默认名的编号 ────────────────────────────────────────────────

// TestNextDraftSeq_SmallestFree 覆盖"最小未占用正整数"这条规则。
//
// 它对应的是一个具体缺陷：用"数量 + 1"算编号时，删掉草稿 2 之后再新建
// 会得到第二条"草稿 2"，目录里出现两条同名。
func TestNextDraftSeq_SmallestFree(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	n, err := db.NextDraftSeq(ctx)
	if err != nil {
		t.Fatalf("NextDraftSeq(空库): %v", err)
	}
	if n != 1 {
		t.Fatalf("空库应给 1，得到 %d", n)
	}

	var ids []int64
	for i := 1; i <= 3; i++ {
		id, err := db.CreateDraft(ctx, "草稿", int64(i))
		if err != nil {
			t.Fatalf("CreateDraft: %v", err)
		}
		ids = append(ids, id)
	}
	if n, _ = db.NextDraftSeq(ctx); n != 4 {
		t.Fatalf("1..3 占用后应给 4，得到 %d", n)
	}

	// 归档**仍然占号**：它还能恢复，让新草稿占它的号会造成同名。
	if err := db.ArchiveDraft(ctx, ids[1]); err != nil {
		t.Fatalf("ArchiveDraft: %v", err)
	}
	if n, _ = db.NextDraftSeq(ctx); n != 4 {
		t.Fatalf("归档的草稿仍应占号，期望 4，得到 %d", n)
	}

	// 彻底删掉之后号才释放。
	if _, err := db.PurgeDraft(ctx, ids[1]); err != nil {
		t.Fatalf("PurgeDraft: %v", err)
	}
	if n, _ = db.NextDraftSeq(ctx); n != 2 {
		t.Fatalf("释放编号 2 之后应给 2（最小未占用），得到 %d", n)
	}
}

// ── 正文 blob 引用 ──────────────────────────────────────────────

// TestDraftBlobRefsInMD 覆盖引用抽取的四种输入。
//
// 第 3 条（转义的 `]` 后面跟括号）是这套文本扫描的**已知攻击面**：
// 生成 md 的那一侧必须把正文里的 `]` 转义掉，否则用户在正文里打一个
// `](` 就能骗过扫描器。前端的 escapeText 保证这一点，
// test/check-md.mjs 里有一条对应用例盯着它。
func TestDraftBlobRefsInMD(t *testing.T) {
	cases := []struct {
		name string
		md   string
		want []string
	}{
		{
			name: "普通图片",
			md:   "正文\n\n![截图](blob/9f/2a/abc.png)\n",
			want: []string{"9f/2a/abc.png"},
		},
		{
			name: "两张图 + 一个普通链接",
			md:   "![a](blob/11/22/a.png) 中间 [链接](https://example.com) ![b](blob/33/44/b.png)",
			want: []string{"11/22/a.png", "33/44/b.png"},
		},
		{
			name: "被转义的方括号不构成链接",
			md:   `正文 \](blob/9f/2a/假的.png) 后面`,
			want: nil,
		},
		{
			name: "包内写法（blobs/）不算库内引用",
			md:   "![a](blobs/9f/2a/abc.png)",
			want: nil,
		},
		{
			name: "未闭合的括号不吞掉后面的内容",
			md:   "![a](blob/9f/2a/x.png 然后 ![b](blob/11/22/y.png)",
			// 第一处按"URL 到空白为止"截断，因此仍然算一个引用。
			// 这是**刻意的保守方向**：多算一个引用只会让文件留着，
			// 少算一个才会把用户的图删掉。
			want: []string{"9f/2a/x.png", "11/22/y.png"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DraftBlobRefsInMD(c.md)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("抽取结果 = %v，期望 %v", got, c.want)
			}
		})
	}
}

// TestDraftBlobRefsRoundTrip 验证导出/导入两侧的重写是互逆的。
func TestDraftBlobRefsRoundTrip(t *testing.T) {
	md := "文字 [链接](https://example.com) ![图](blob/9f/2a/abc.png)"
	inPkg := DraftBlobRefsToPackage(md)
	if !strings.Contains(inPkg, "](blobs/9f/2a/abc.png)") {
		t.Fatalf("导出改写后应当出现包内路径，得到 %q", inPkg)
	}
	if !strings.Contains(inPkg, "](https://example.com)") {
		t.Fatalf("普通链接不该被改写，得到 %q", inPkg)
	}

	back, dropped := DraftBlobRefsFromPackage(inPkg, func(rel string) bool { return true })
	if dropped != 0 {
		t.Fatalf("全部保留时 dropped 应为 0，得到 %d", dropped)
	}
	if back != md {
		t.Fatalf("往返之后应完全一致：\n原始 %q\n回来 %q", md, back)
	}

	// 包里缺这个 blob 时，引用被摘掉并计数。
	pruned, dropped := DraftBlobRefsFromPackage(inPkg, func(string) bool { return false })
	if dropped != 1 {
		t.Fatalf("缺 blob 时应计数 1，得到 %d", dropped)
	}
	if strings.Contains(pruned, "blobs/") || strings.Contains(pruned, "blob/") {
		t.Fatalf("缺 blob 的引用应当被摘掉，得到 %q", pruned)
	}
}

// TestAllBlobRefs_IncludesDrafts 是那个既有缺陷的回归测试。
//
// 缺陷原样：AllBlobRefs 只扫 items，于是草稿里贴的图片在超过一天后的
// 第一次 GC 里被当孤儿删掉（"草稿里的图偶发消失"）。
func TestAllBlobRefs_IncludesDrafts(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	img := "aa/bb/item-image.png"
	thumb := "aa/bb/item-image.thumb.png"
	item := sampleItem("sha256:"+strings.Repeat("a", 64), "带图条目")
	item.ImagePath, item.ThumbPath = &img, &thumb
	if _, err := PutItem(ctx, db.Writer(), &item); err != nil {
		t.Fatalf("PutItem: %v", err)
	}

	draftRel := "cc/dd/draft-image.png"
	id, err := db.CreateDraft(ctx, "草稿1", 1)
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	md := "一段正文\n\n![贴图](" + DraftBlobPrefix + draftRel + ")\n"
	if _, err := db.SaveDraftMD(ctx, id, md); err != nil {
		t.Fatalf("SaveDraftMD: %v", err)
	}

	refs, err := db.AllBlobRefs(ctx)
	if err != nil {
		t.Fatalf("AllBlobRefs: %v", err)
	}
	for _, want := range []string{img, thumb, draftRel} {
		if _, ok := refs[want]; !ok {
			t.Errorf("引用集合里缺少 %q（共 %d 项）", want, len(refs))
		}
	}

	// 归档（软删除）的草稿同样算引用：它还能恢复。
	if err := db.ArchiveDraft(ctx, id); err != nil {
		t.Fatalf("ArchiveDraft: %v", err)
	}
	refs, err = db.AllBlobRefs(ctx)
	if err != nil {
		t.Fatalf("AllBlobRefs(归档后): %v", err)
	}
	if _, ok := refs[draftRel]; !ok {
		t.Error("归档的草稿仍应算引用（它的图不能当孤儿删）")
	}
}

// ── 保存路径 ────────────────────────────────────────────────────

// TestSaveDraftMD_DoesNotTouchTitle 钉住"实时保存不碰标题"这条约定：
// 两条写路径分开，才不会出现"自动保存顺手把用户正在改的标题覆盖回去"。
func TestSaveDraftMD_DoesNotTouchTitle(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	id, err := db.CreateDraft(ctx, "草稿1", 1)
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := db.RenameDraft(ctx, id, "会议记录"); err != nil {
		t.Fatalf("RenameDraft: %v", err)
	}
	if _, err := db.SaveDraftMD(ctx, id, "正文"); err != nil {
		t.Fatalf("SaveDraftMD: %v", err)
	}

	d, err := db.GetDraft(ctx, id)
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if d.Title != "会议记录" {
		t.Fatalf("标题被保存路径改动了：%q", d.Title)
	}
	if d.MD != "正文" {
		t.Fatalf("正文 = %q，期望 正文", d.MD)
	}
}

// TestSaveDraftMD_Gone 验证"草稿在编辑期间被删掉"会得到一个可识别的错误。
//
// 前端据此提示"这条草稿已被删除"，而不是"保存失败，请重试"——
// 后者会让用户一直重试一件永远不会成功的事。
func TestSaveDraftMD_Gone(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	id, err := db.CreateDraft(ctx, "草稿1", 1)
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if _, err := db.PurgeDraft(ctx, id); err != nil {
		t.Fatalf("PurgeDraft: %v", err)
	}
	if _, err := db.SaveDraftMD(ctx, id, "x"); err == nil {
		t.Fatal("对已删除的草稿保存应当报错")
	} else if !strings.Contains(err.Error(), "draft not found") {
		t.Fatalf("错误应当能被识别为 ErrDraftGone，得到：%v", err)
	}
}

// TestPurgeArchivedDraftsBefore 覆盖归档到期回收。
func TestPurgeArchivedDraftsBefore(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	keep, _ := db.CreateDraft(ctx, "新删的", 1)
	old, _ := db.CreateDraft(ctx, "删很久了", 2)
	if _, err := db.SaveDraftMD(ctx, old, "很长的一段正文"); err != nil {
		t.Fatalf("SaveDraftMD: %v", err)
	}
	for _, id := range []int64{keep, old} {
		if err := db.ArchiveDraft(ctx, id); err != nil {
			t.Fatalf("ArchiveDraft: %v", err)
		}
	}
	// 把 old 的归档时间改到 30 天前。
	if _, err := db.w.ExecContext(ctx,
		`UPDATE drafts SET archived_at = ? WHERE id = ?`, 1000, old); err != nil {
		t.Fatalf("改归档时间: %v", err)
	}

	purged, chars, err := db.PurgeArchivedDraftsBefore(ctx, 2000)
	if err != nil {
		t.Fatalf("PurgeArchivedDraftsBefore: %v", err)
	}
	if purged != 1 {
		t.Fatalf("应删 1 条，得到 %d", purged)
	}
	// 单位是**字符**不是字节（SQLite 的 length() 对 TEXT 按字符计）：
	// 正文里中文占多数，按字节算会让"释放了多少"这个数字看起来虚高。
	if want := int64(utf8.RuneCountInString("很长的一段正文")); chars != want {
		t.Fatalf("释放字符数 = %d，期望 %d", chars, want)
	}
	arch := archivedTitles(t, db)
	if !reflect.DeepEqual(arch, []string{"新删的"}) {
		t.Fatalf("归档区剩下 %v，期望 [新删的]", arch)
	}
}

// ── 摘要 ────────────────────────────────────────────────────────

func TestDraftSnippet(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"纯文本原样", "今天开了个会", "今天开了个会"},
		{"换行压成空格", "第一行\n第二行", "第一行 第二行"},
		{"去掉图片整体", "![图](blob/9f/2a/x.png)", ""},
		{"链接只留文字", "见 [这篇文档](https://example.com/doc)", "见 这篇文档"},
		{"去掉强调标记", "**重点**和*斜体*", "重点和斜体"},
		{"超长截断加省略号", strings.Repeat("字", snippetChars+10), strings.Repeat("字", snippetChars) + "…"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DraftSnippet(c.in); got != c.want {
				t.Fatalf("DraftSnippet(%q) = %q，期望 %q", c.in, got, c.want)
			}
		})
	}
}

// ── 备份：导出读 / 导入写（docs/BACKUP-FORMAT.md §3.7）───────────

// TestDraftBlobRefsInPackageMD 钉住包内前缀（blobs/）的扫描，
// 以及它与库内前缀（blob/）**互不串味**。
//
// 两个前缀只差一个 s，扫错一个的后果很具体：导出时漏掉图片（包不完整），
// 或者导入时把 `blobs/x` 当成外部链接留在正文里（必然破图且无人报错）。
func TestDraftBlobRefsInPackageMD(t *testing.T) {
	cases := []struct {
		name string
		md   string
		want []string
	}{
		{"包内前缀", "![a](blobs/9f/2a/aa.png)", []string{"9f/2a/aa.png"}},
		{"多个", "![a](blobs/9f/2a/aa.png) 文字 [b](blobs/3b/07/bb.rtf)",
			[]string{"9f/2a/aa.png", "3b/07/bb.rtf"}},
		{"库内前缀不认", "![a](blob/9f/2a/aa.png)", nil},
		{"外链不认", "[x](https://example.com/blobs/a.png)", nil},
		{"空前缀", "![a](blobs/)", nil},
		// 转义的 `]` 不构成链接：用户在正文里打一个 `](` 不该骗过扫描器。
		{"转义的方括号", `文字 \](blobs/9f/2a/aa.png) 还在文字里`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DraftBlobRefsInPackageMD(c.md)
			if len(got) != len(c.want) {
				t.Fatalf("得到 %v，期望 %v", got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("得到 %v，期望 %v", got, c.want)
				}
			}
		})
	}
}

// TestInsertImportedDraft 钉住导入草稿与新建草稿的三点差别：
// seq 不占本机编号、sort_order 追加到末尾、时间保留源机器的。
func TestInsertImportedDraft(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	// 本机已有两条草稿，编号 1、2。
	if _, err := db.CreateDraft(ctx, "本机一", 1); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if _, err := db.CreateDraft(ctx, "本机二", 2); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}

	importID, err := db.CreateImport(ctx, "p.clipbak", "sha256:deadbeef")
	if err != nil {
		t.Fatalf("CreateImport: %v", err)
	}
	id, err := db.InsertImportedDraft(ctx, db.Writer(), importID, &ImportedDraft{
		Title: "外来的", MD: "正文", CreatedAt: 1_700_000_000, UpdatedAt: 1_700_000_500,
	})
	if err != nil {
		t.Fatalf("InsertImportedDraft: %v", err)
	}

	dr, err := db.GetDraft(ctx, id)
	if err != nil || dr == nil {
		t.Fatalf("GetDraft(%d) = %v, %v", id, dr, err)
	}
	if dr.Seq != 0 {
		t.Errorf("Seq = %d，期望 0（导入的草稿不该占本机编号，否则会撞出两条「草稿 1」）", dr.Seq)
	}
	if dr.Title != "外来的" || dr.MD != "正文" {
		t.Errorf("标题/正文 = %q/%q，期望 外来的/正文", dr.Title, dr.MD)
	}
	if dr.CreatedAt != 1_700_000_000 || dr.UpdatedAt != 1_700_000_500 {
		t.Errorf("时间 = %d/%d，期望保留源机器的 1700000000/1700000500",
			dr.CreatedAt, dr.UpdatedAt)
	}

	// 追加到末尾：目录顺序里它排在本机两条之后。
	got := titles(t, db)
	want := []string{"本机一", "本机二", "外来的"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("目录顺序 = %v，期望 %v", got, want)
	}

	// 下一号仍然是 3（导入的草稿没有占用编号）。
	next, err := db.NextDraftSeq(ctx)
	if err != nil {
		t.Fatalf("NextDraftSeq: %v", err)
	}
	if next != 3 {
		t.Errorf("下一号 = %d，期望 3", next)
	}
}

// TestStreamAliveDrafts_SkipsArchived 钉住"导出只带存活草稿"，
// 且流式读的条数与 CountDraftsTx 的存活数一致（stats.drafts 的依据）。
func TestStreamAliveDrafts_SkipsArchived(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	ids := make([]int64, 0, 3)
	for i, title := range []string{"甲", "乙", "丙"} {
		id, err := db.CreateDraft(ctx, title, int64(i+1))
		if err != nil {
			t.Fatalf("CreateDraft: %v", err)
		}
		ids = append(ids, id)
	}
	if err := db.ArchiveDraft(ctx, ids[1]); err != nil {
		t.Fatalf("ArchiveDraft: %v", err)
	}

	alive, archived, err := db.CountDraftsTx(ctx, db.Reader())
	if err != nil {
		t.Fatalf("CountDraftsTx: %v", err)
	}
	if alive != 2 || archived != 1 {
		t.Fatalf("计数 = %d/%d，期望 2/1", alive, archived)
	}

	var got []string
	n, err := db.StreamAliveDrafts(ctx, db.Reader(), func(d *Draft) error {
		got = append(got, d.Title)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamAliveDrafts: %v", err)
	}
	if n != alive {
		t.Errorf("流式读到 %d 条，CountDraftsTx 说 %d 条（stats.drafts 会与实际写出的不符）", n, alive)
	}
	if !reflect.DeepEqual(got, []string{"甲", "丙"}) {
		t.Fatalf("读到 %v，期望 [甲 丙]（归档的不该出现）", got)
	}

	// 回调里返回错误要能立刻中断并原样传出（调用方靠它中止导出）。
	sentinel := errStopStream
	if _, err := db.StreamAliveDrafts(ctx, db.Reader(), func(d *Draft) error {
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("回调的错误被吞了：%v", err)
	}
}

var errStopStream = errors.New("stop")

// TestDeleteImportItems_AlsoRemovesDrafts 钉住一键回滚的**完整性**。
//
// 只删 items 不删 drafts 的后果是隐性的：批次被标成 rolled_back，
// 用户再也找不到入口去撤掉那批草稿——它们会一直留在目录里，
// 而用户以为自己已经回滚干净了。
func TestDeleteImportItems_AlsoRemovesDrafts(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	importID, err := db.CreateImport(ctx, "p.clipbak", "sha256:feedface")
	if err != nil {
		t.Fatalf("CreateImport: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := db.InsertImportedDraft(ctx, db.Writer(), importID, &ImportedDraft{
			Title: "导入的", MD: "正文", CreatedAt: 1_700_000_000,
		}); err != nil {
			t.Fatalf("InsertImportedDraft: %v", err)
		}
	}
	// 一条不属于这个批次的草稿必须留下。
	keep, err := db.CreateDraft(ctx, "本机的", 1)
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}

	if _, err := db.DeleteImportItems(ctx, importID); err != nil {
		t.Fatalf("DeleteImportItems: %v", err)
	}
	left := titles(t, db)
	if !reflect.DeepEqual(left, []string{"本机的"}) {
		t.Fatalf("回滚后剩 %v，期望只有 [本机的]", left)
	}
	if _, err := db.GetDraft(ctx, keep); err != nil {
		t.Fatalf("本机草稿被误删：%v", err)
	}

	alive, _, err := db.CountDraftsTx(ctx, db.Reader())
	if err != nil {
		t.Fatalf("CountDraftsTx: %v", err)
	}
	if alive != 1 {
		t.Fatalf("回滚后存活草稿 = %d，期望 1", alive)
	}
}

// TestPurgeDraft_DetachesFromImport 是 import_id 外键的健全性检查：
// 硬删一条导入的草稿不能连带影响别的行。
func TestPurgeDraft_DetachesFromImport(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	importID, err := db.CreateImport(ctx, "p.clipbak", "sha256:0123")
	if err != nil {
		t.Fatalf("CreateImport: %v", err)
	}
	id, err := db.InsertImportedDraft(ctx, db.Writer(), importID, &ImportedDraft{
		Title: "导入的", MD: "x", CreatedAt: 1_700_000_000,
	})
	if err != nil {
		t.Fatalf("InsertImportedDraft: %v", err)
	}
	if n, err := db.PurgeDraft(ctx, id); err != nil || n != 1 {
		t.Fatalf("PurgeDraft = %d, %v；期望 1, nil", n, err)
	}
	// 批次行还在（只是它的草稿没了）。
	if row, err := db.GetImport(ctx, importID); err != nil || row == nil {
		t.Fatalf("批次行不该被带走：%v, %v", row, err)
	}
}

// TestListDrafts_DoesNotLoadFullBody 钉住"列表不读正文全文"这条纪律。
//
// 它用摘要长度间接验证：列表拿到的是 substr(md,1,120) 而不是完整正文。
// 直接断言"没读全文"是做不到的，但拿一条远超摘要长度的正文，
// 断言 Snippet 被截断、且 Chars 反映真实长度，就足以证明两列分开取了。
func TestListDrafts_DoesNotLoadFullBody(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	id, _ := db.CreateDraft(ctx, "长文", 1)
	body := strings.Repeat("长", 5000)
	if _, err := db.SaveDraftMD(ctx, id, body); err != nil {
		t.Fatalf("SaveDraftMD: %v", err)
	}
	rows, err := db.ListDrafts(ctx)
	if err != nil {
		t.Fatalf("ListDrafts: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应有一条，得到 %d", len(rows))
	}
	if got := len([]rune(rows[0].Snippet)); got > snippetChars+1 {
		t.Fatalf("摘要长度 %d 超出上限 %d（说明列表把全文读进来了）", got, snippetChars)
	}
	if rows[0].Chars != 5000 {
		t.Fatalf("Chars = %d，期望 5000", rows[0].Chars)
	}
}
