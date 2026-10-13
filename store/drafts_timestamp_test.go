package store

import (
	"context"
	"errors"
	"testing"
)

// ── updated_at 的语义：最近**修改**时间 ──────────────────────────
//
// 这一组用例钉住一条不变量：**内容没变，updated_at 就不许动**。
//
// 它来自一个真实的用户报障：点开一篇草稿看一眼，列表里的时间就变成"刚刚"。
// 根因有两层 ——
//
//  1. 前端切草稿时会先 `flush({force:true})` 把上一篇结算一次
//     （frontend/src/views/Drafts.tsx 的 openDraft）；
//  2. SaveDraftMD / RenameDraft 是**无条件** UPDATE：写回同样的字节也会把
//     updated_at 顶成 now。
//
// 第 2 层是真正要在这里钉死的那一层。调用方怎么看待"有没有变"都不该让
// 时间戳失真，而**库里那份内容才是权威**这件事只有库自己知道。
//
// 做法是把比对放进 SQL 的 WHERE（`AND md <> ?`），而不是靠 RowsAffected：
// SQLite 的 changes() 数的是**匹配上的行**，把同一份内容再写一次也一样是 1
// 行 —— 靠它判断"有没有真的变化"必然错。
//
// 一致时一个字节都不写，并把**原来的** updated_at 原样返回：前端拿它显示
// "已保存 · HH:MM"，不能显示成刚刚。

// pinDraftTimes 把时间列钉到一个可辨认的旧值上，模拟"这篇草稿上次修改是很久
// 以前"。测试自己写库（而不是等真实时间流逝）才能断言"时间没有动"。
func pinDraftTimes(t *testing.T, db *DB, id, ts int64) {
	t.Helper()
	if _, err := db.w.ExecContext(context.Background(),
		`UPDATE drafts SET created_at = ?, updated_at = ? WHERE id = ?`, ts, ts, id); err != nil {
		t.Fatalf("pin times: %v", err)
	}
}

func draftUpdatedAt(t *testing.T, db *DB, id int64) int64 {
	t.Helper()
	var ts int64
	if err := db.r.QueryRowContext(context.Background(),
		`SELECT updated_at FROM drafts WHERE id = ?`, id).Scan(&ts); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	return ts
}

// TestSaveDraftMD_UnchangedBodyKeepsUpdatedAt 是本组的核心：同一份正文再存
// 一次（"点开看一眼"、"切走时结算一次"都会走到这里），库里时间必须原样不动，
// 返回值也必须是**原来那个**时间 —— 前端会拿它更新列表与"已保存"。
func TestSaveDraftMD_UnchangedBodyKeepsUpdatedAt(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	id, err := db.CreateDraft(ctx, "标题", 1)
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	const body = "正文：这一段不会被改动。"
	if _, err := db.SaveDraftMD(ctx, id, body); err != nil {
		t.Fatalf("SaveDraftMD: %v", err)
	}
	const pinned = 1_600_000_000
	pinDraftTimes(t, db, id, pinned)

	got, err := db.SaveDraftMD(ctx, id, body)
	if err != nil {
		t.Fatalf("SaveDraftMD（同内容）: %v", err)
	}
	if got != pinned {
		t.Errorf("同内容保存返回 updated_at = %d，期望 %d（内容没变，时间不该动）", got, pinned)
	}
	if after := draftUpdatedAt(t, db, id); after != pinned {
		t.Errorf("同内容保存后库里的 updated_at = %d，期望 %d", after, pinned)
	}
}

// TestSaveDraftMD_ChangedBodyMovesUpdatedAt 是上一条的反面：内容真的变了，
// 时间必须跟着走。没有这一条，"不改时间"用一句 `UPDATE` 都不写也能骗过去。
func TestSaveDraftMD_ChangedBodyMovesUpdatedAt(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	id, err := db.CreateDraft(ctx, "标题", 1)
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if _, err := db.SaveDraftMD(ctx, id, "旧正文"); err != nil {
		t.Fatalf("SaveDraftMD: %v", err)
	}
	const pinned = 1_600_000_000
	pinDraftTimes(t, db, id, pinned)

	got, err := db.SaveDraftMD(ctx, id, "新正文")
	if err != nil {
		t.Fatalf("SaveDraftMD（新内容）: %v", err)
	}
	if got <= pinned {
		t.Errorf("改了内容，返回的 updated_at = %d，期望 > %d", got, pinned)
	}
	if after := draftUpdatedAt(t, db, id); after != got {
		t.Errorf("库里的 updated_at = %d，期望与返回值一致（%d）", after, got)
	}
}

// TestSaveDraftMD_EmptyBodyIsARealChange：把正文删光也是修改。
//
// 空串是合法状态（见 SaveDraftMD 的注释），而 `md <> ”` 在"本来就不是空"
// 时成立 —— 但这一条要钉住的是**返回空串**那条路径没有被"没变化"的短路
// 顺手吞掉。
func TestSaveDraftMD_EmptyBodyIsARealChange(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	id, err := db.CreateDraft(ctx, "标题", 1)
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if _, err := db.SaveDraftMD(ctx, id, "有内容"); err != nil {
		t.Fatalf("SaveDraftMD: %v", err)
	}
	const pinned = 1_600_000_000
	pinDraftTimes(t, db, id, pinned)

	got, err := db.SaveDraftMD(ctx, id, "")
	if err != nil {
		t.Fatalf("SaveDraftMD（清空）: %v", err)
	}
	if got <= pinned {
		t.Errorf("清空正文后 updated_at = %d，期望 > %d（清空是一次真实的修改）", got, pinned)
	}
}

// TestSaveDraftMD_StillReportsGoneDraft：比对进了 WHERE 之后，"内容没变"与
// "这一行已经不在了"都会让 RowsAffected 为 0 —— 后者必须仍然报 ErrDraftGone，
// 前端据此提示"草稿已被删除"，而不是显示"已保存"。
func TestSaveDraftMD_StillReportsGoneDraft(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	if _, err := db.SaveDraftMD(ctx, 9999, "写不进去"); !errors.Is(err, ErrDraftGone) {
		t.Fatalf("对不存在的草稿写入 = %v，期望 ErrDraftGone", err)
	}
}

// TestRenameDraft_OnlyRealChangesMoveUpdatedAt：标题同理。
// 点开标题框、没改就移开焦点（或改回原样）不该让时间跳到"刚刚"。
func TestRenameDraft_OnlyRealChangesMoveUpdatedAt(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	id, err := db.CreateDraft(ctx, "原标题", 1)
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	const pinned = 1_600_000_000
	pinDraftTimes(t, db, id, pinned)

	if err := db.RenameDraft(ctx, id, "原标题"); err != nil {
		t.Fatalf("RenameDraft（同标题）: %v", err)
	}
	if after := draftUpdatedAt(t, db, id); after != pinned {
		t.Errorf("同标题重命名后 updated_at = %d，期望 %d", after, pinned)
	}

	if err := db.RenameDraft(ctx, id, "新标题"); err != nil {
		t.Fatalf("RenameDraft（新标题）: %v", err)
	}
	if after := draftUpdatedAt(t, db, id); after <= pinned {
		t.Errorf("换了标题，updated_at = %d，期望 > %d", after, pinned)
	}
}

// TestRenameDraft_StillReportsGoneDraft：同 SaveDraftMD，不能把"行不在了"
// 吞成"没变化"。
func TestRenameDraft_StillReportsGoneDraft(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	if err := db.RenameDraft(ctx, 9999, "标题"); !errors.Is(err, ErrDraftGone) {
		t.Fatalf("对不存在的草稿改名 = %v，期望 ErrDraftGone", err)
	}
}
