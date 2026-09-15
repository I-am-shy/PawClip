// Package store 是持久化层：SQLite schema、条目写入路径、blob 文件、设置。
//
// 三条硬约束（DESIGN.md §2 / §14，HANDOFF-PROMPT §4）：
//
//  1. **写必须串行**。本包暴露两个句柄：w（单连接，只被 Writer goroutine 触碰）
//     与 r（连接池，只跑 SELECT）。WAL 下读不阻塞写，写不阻塞读。
//  2. **FTS5 保留默认 detail=full**。detail=none / detail=column 与 trigram
//     分词器不兼容——建表与回填都会成功，查询却直接抛 OperationalError。
//  3. **启动不做无条件的 integrity_check**。改用 clean_shutdown 标记文件：
//     正常退出删标记，启动时发现标记存在才做完整性检查。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// SchemaVersion 是当前 schema 版本，写在 PRAGMA user_version 里。
// 每次不兼容变更 +1，并在 migrations 里追加一步。
const SchemaVersion = 1

// cleanShutdownMarker 是"上次没有正常退出"的标记文件名（放在数据库同目录）。
const cleanShutdownMarker = "clean_shutdown"

// 驱动名。不直接用 "sqlite3"，是为了挂 ConnectHook 把 PRAGMA 打到
// **每一条新连接**上（busy_timeout / foreign_keys / synchronous 都是
// 连接级设置，只在池里的第一条连接上设会留坑）。
const (
	driverRW = "pawclip_sqlite3_rw"
	driverRO = "pawclip_sqlite3_ro"
)

// 错误。
var (
	// ErrSchemaTooNew 表示库的 user_version 高于本程序支持的版本。
	ErrSchemaTooNew = errors.New("store: database schema is newer than this build")
)

// Options 是 Open 的入参。
type Options struct {
	// Path 是数据库文件路径。目录会被自动创建（权限 0700）。
	Path string
	// Logger 为 nil 时用 slog.Default()。
	Logger *slog.Logger
	// ReadMaxConns 是读连接池上限，<=0 取 4。
	ReadMaxConns int
	// SkipMigration 仅供迁移测试使用。
	SkipMigration bool
}

// DB 持有两个 SQLite 句柄与运行时状态。
type DB struct {
	w    *sql.DB // 单写：SetMaxOpenConns(1)，只由 Writer goroutine 使用
	r    *sql.DB // 读：连接池
	path string
	dir  string
	log  *slog.Logger

	ftsAvailable bool
	// integrityNote 记录启动自检发现的问题，供上层展示（空串表示没问题）。
	integrityNote string
	// markerEnabled 对应设置 storage.cleanShutdownMarker。
	markerEnabled bool

	closeOnce sync.Once
}

// Open 打开（必要时创建）数据库，完成建表/迁移/启动自检。
func Open(opts Options) (*DB, error) {
	if strings.TrimSpace(opts.Path) == "" {
		return nil, errors.New("store: empty database path")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	readConns := opts.ReadMaxConns
	if readConns <= 0 {
		readConns = 4
	}

	dir := filepath.Dir(opts.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: create data dir %s: %w", dir, err)
	}

	registerDrivers()

	markerPath := filepath.Join(dir, cleanShutdownMarker)
	hadMarker := fileExists(markerPath)

	w, err := sql.Open(driverRW, opts.Path)
	if err != nil {
		return nil, fmt.Errorf("store: open write handle: %w", err)
	}
	// 写串行化的最后一道保险：即使别处误用 w，也只会有一条连接。
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)

	d := &DB{
		w:             w,
		path:          opts.Path,
		dir:           dir,
		log:           log,
		markerEnabled: true,
	}

	// journal_mode 是持久设置（写在库文件头里），只在打开时设一次。
	// 它不能在事务里执行，所以必须在迁移之前。
	if err := d.execOne("PRAGMA journal_mode = WAL"); err != nil {
		w.Close()
		return nil, err
	}

	if !opts.SkipMigration {
		if err := d.migrate(); err != nil {
			w.Close()
			return nil, err
		}
	}
	if err := d.ensureFTS(); err != nil {
		w.Close()
		return nil, err
	}

	// 启动自检：标记文件存在 → 上次是异常退出 → 才值得花几秒做 integrity_check。
	if hadMarker {
		d.integrityNote = d.runIntegrityCheck()
		if d.integrityNote != "" {
			log.Error("database integrity check reported problems",
				"path", opts.Path, "detail", d.integrityNote)
		} else {
			log.Info("clean_shutdown marker found and integrity check passed", "path", opts.Path)
		}
	} else {
		log.Debug("no clean_shutdown marker; skipping integrity check")
	}

	ro, err := sql.Open(driverRO, opts.Path)
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("store: open read handle: %w", err)
	}
	ro.SetMaxOpenConns(readConns)
	ro.SetMaxIdleConns(readConns)
	d.r = ro

	// FTS 自检（DESIGN.md §13 风险表：失败则整体退化为 LIKE 模式）。
	// 必须放在读句柄打开之后——自检走的是查询路径。
	d.ftsAvailable = d.checkFTS()

	// 迁移完成后写入标记：进程活着就代表"可能没正常退出"。
	if err := d.writeMarker(); err != nil {
		log.Warn("cannot write clean_shutdown marker", "err", err)
	}
	return d, nil
}

func registerDrivers() {
	driverOnce.Do(func() {
		sql.Register(driverRW, &sqlite3.SQLiteDriver{
			ConnectHook: func(c *sqlite3.SQLiteConn) error {
				return applyPragmas(c, false)
			},
		})
		sql.Register(driverRO, &sqlite3.SQLiteDriver{
			ConnectHook: func(c *sqlite3.SQLiteConn) error {
				return applyPragmas(c, true)
			},
		})
	})
}

var driverOnce sync.Once

// applyPragmas 在每条新连接上设置连接级 PRAGMA。
func applyPragmas(c *sqlite3.SQLiteConn, readOnly bool) error {
	pragmas := []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA synchronous = NORMAL",
	}
	if readOnly {
		// 读句柄只跑 SELECT；打开 query_only 让误写立即报错而不是悄悄绕开单写纪律。
		pragmas = append(pragmas, "PRAGMA query_only = ON")
	}
	for _, p := range pragmas {
		if _, err := c.Exec(p, nil); err != nil {
			return fmt.Errorf("store: %s: %w", p, err)
		}
	}
	return nil
}

// ── 迁移 ────────────────────────────────────────────────────────

// migrations[i] 把 user_version 从 i 升到 i+1。
var migrations = []func(*sql.Tx) error{
	// 0 → 1：全量建表（DESIGN.md §4.1）
	func(tx *sql.Tx) error {
		_, err := tx.Exec(ddlV1)
		return err
	},
}

func (d *DB) migrate() error {
	var v int
	if err := d.w.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("store: read user_version: %w", err)
	}
	switch {
	case v == SchemaVersion:
		return nil
	case v > SchemaVersion:
		return fmt.Errorf("%w (db=%d build=%d)", ErrSchemaTooNew, v, SchemaVersion)
	}

	for from := v; from < SchemaVersion; from++ {
		step := migrations[from]
		if step == nil {
			return fmt.Errorf("store: missing migration %d → %d", from, from+1)
		}
		tx, err := d.w.Begin()
		if err != nil {
			return fmt.Errorf("store: begin migration %d: %w", from, err)
		}
		if err := step(tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %d → %d: %w", from, from+1, err)
		}
		// PRAGMA 不接受占位符，只能拼字面量；from+1 是我们自己的常量，无注入面。
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", from+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: set user_version %d: %w", from+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit migration %d: %w", from, err)
		}
		d.log.Info("database migrated", "from", from, "to", from+1)
	}
	return nil
}

// ddlV1 是 DESIGN.md §4.1 的建表 DDL（FTS 表与触发器见 ensureFTS）。
//
// 父表先建，子表后建，避免外键引用悬空。
const ddlV1 = `
CREATE TABLE IF NOT EXISTS categories (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  color       TEXT,
  icon        TEXT,
  rule        TEXT,
  ttl_seconds INTEGER,
  sort_order  INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS imports (
  id            INTEGER PRIMARY KEY,
  source_name   TEXT,
  manifest_hash TEXT,
  started_at    INTEGER NOT NULL,
  finished_at   INTEGER,
  imported      INTEGER NOT NULL DEFAULT 0,
  skipped       INTEGER NOT NULL DEFAULT 0,
  failed        INTEGER NOT NULL DEFAULT 0,
  status        TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS items (
  id              INTEGER PRIMARY KEY,
  kind            TEXT    NOT NULL,
  text_content    TEXT,
  html_content    TEXT,
  rtf_path        TEXT,
  image_path      TEXT,
  thumb_path      TEXT,
  file_paths      TEXT,
  preview         TEXT,
  fingerprint     TEXT    NOT NULL,
  byte_size       INTEGER NOT NULL DEFAULT 0,
  source_app_id   TEXT,
  source_app_name TEXT,
  source_url      TEXT,
  category_id     INTEGER REFERENCES categories(id) ON DELETE SET NULL,
  pinned          INTEGER NOT NULL DEFAULT 0,
  first_seen_at   INTEGER NOT NULL,
  expires_at      INTEGER,
  ttl_source      TEXT,
  created_at      INTEGER NOT NULL,
  last_used_at    INTEGER,
  use_count       INTEGER NOT NULL DEFAULT 0,
  deleted_at      INTEGER,
  import_id       INTEGER REFERENCES imports(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_items_alive  ON items(created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_items_expire ON items(expires_at) WHERE expires_at IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_items_app    ON items(source_app_id);
CREATE INDEX IF NOT EXISTS idx_items_cat    ON items(category_id);
CREATE INDEX IF NOT EXISTS idx_items_import ON items(import_id);

-- 只对"存活条目"做指纹唯一，删掉后可重新录入同一内容（DESIGN.md §4.1）
CREATE UNIQUE INDEX IF NOT EXISTS uq_items_fp_alive ON items(fingerprint) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS tags (
  id    INTEGER PRIMARY KEY,
  name  TEXT NOT NULL UNIQUE,
  color TEXT
);

CREATE TABLE IF NOT EXISTS item_tags (
  item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  tag_id  INTEGER NOT NULL REFERENCES tags(id)  ON DELETE CASCADE,
  PRIMARY KEY (item_id, tag_id)
);

CREATE TABLE IF NOT EXISTS settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
`

// ftsDDL 是与 items 同步的 FTS5 表。
//
// ⚠️ 不要给 tokenize=trigram 加 detail=none / detail=column：建表与回填都会
// 成功，但查询直接抛 OperationalError（DESIGN.md §14 第 8 条，已实测）。
const ftsDDL = `
CREATE VIRTUAL TABLE IF NOT EXISTS items_fts USING fts5(
  text_content,
  preview,
  content       = 'items',
  content_rowid = 'id',
  tokenize      = 'trigram'
);
`

// ftsTriggersDDL 是 AFTER INSERT / UPDATE / DELETE 三个同步触发器。
//
// external content 表必须用 'delete' 命令形式喂旧值，直接 DELETE 是不行的。
const ftsTriggersDDL = `
CREATE TRIGGER IF NOT EXISTS items_ai AFTER INSERT ON items BEGIN
  INSERT INTO items_fts(rowid, text_content, preview) VALUES (new.id, new.text_content, new.preview);
END;
CREATE TRIGGER IF NOT EXISTS items_ad AFTER DELETE ON items BEGIN
  INSERT INTO items_fts(items_fts, rowid, text_content, preview) VALUES('delete', old.id, old.text_content, old.preview);
END;
CREATE TRIGGER IF NOT EXISTS items_au AFTER UPDATE ON items BEGIN
  INSERT INTO items_fts(items_fts, rowid, text_content, preview) VALUES('delete', old.id, old.text_content, old.preview);
  INSERT INTO items_fts(rowid, text_content, preview) VALUES (new.id, new.text_content, new.preview);
END;
`

// ensureFTS 建 FTS 表与触发器。
//
// 故意放在版本化迁移**之外**：FTS5 是否可用取决于构建标签，与 schema 版本无关。
// 老库换到带 sqlite_fts5 的构建上时，这里会把索引补起来并回填。
func (d *DB) ensureFTS() error {
	if _, err := d.w.Exec(ftsDDL); err != nil {
		d.log.Warn("FTS5 unavailable, search will fall back to LIKE", "err", err)
		return nil
	}
	if _, err := d.w.Exec(ftsTriggersDDL); err != nil {
		d.log.Warn("cannot create FTS triggers, FTS disabled", "err", err)
		return nil
	}
	// 老库补索引：rebuild 会清空全文索引再从 content=items 重建。
	if _, err := d.w.Exec(`INSERT INTO items_fts(items_fts) VALUES('rebuild')`); err != nil {
		d.log.Warn("FTS rebuild failed; triggers will keep it in sync from now on", "err", err)
	}
	return nil
}

// checkFTS 是启动自检：HANDOFF-PROMPT §4 第 7 条指定的那条查询。
func (d *DB) checkFTS() bool {
	rows, err := d.r.Query("SELECT * FROM items_fts LIMIT 1")
	if err != nil {
		d.log.Warn("FTS self-check failed, degrading to LIKE search", "err", err)
		return false
	}
	rows.Close()
	return true
}

// FTSAvailable 报告全文检索是否可用。false 时上层整体退化为 LIKE 模式。
func (d *DB) FTSAvailable() bool { return d.ftsAvailable }

// runIntegrityCheck 返回空串表示通过，否则返回问题描述。
func (d *DB) runIntegrityCheck() string {
	rows, err := d.w.Query("PRAGMA integrity_check")
	if err != nil {
		return "integrity_check query failed: " + err.Error()
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "integrity_check scan failed: " + err.Error()
		}
		if strings.EqualFold(strings.TrimSpace(line), "ok") {
			continue
		}
		problems = append(problems, line)
		if len(problems) >= 8 {
			problems = append(problems, "…")
			break
		}
	}
	if err := rows.Err(); err != nil {
		return "integrity_check iterate failed: " + err.Error()
	}
	return strings.Join(problems, "; ")
}

// IntegrityNote 返回启动自检发现的问题（空串表示通过或未做检查）。
func (d *DB) IntegrityNote() string { return d.integrityNote }

// ── clean_shutdown 标记 ─────────────────────────────────────────

func (d *DB) markerPath() string { return filepath.Join(d.dir, cleanShutdownMarker) }

func (d *DB) writeMarker() error {
	return os.WriteFile(d.markerPath(), []byte("1\n"), 0o600)
}

// SetCleanShutdownMarkerEnabled 对应设置 storage.cleanShutdownMarker。
//
// 为什么不能在设置加载前应用：设置本身存在这个库里（DESIGN.md §9.0 规定
// settings 表是运行时设置唯一真源），所以"是否用标记"必然是"先按默认 true
// 检查一次，加载到设置后再纠正"。
func (d *DB) SetCleanShutdownMarkerEnabled(enabled bool) error {
	d.markerEnabled = enabled
	if enabled {
		return d.writeMarker()
	}
	return os.Remove(d.markerPath())
}

// ── 事务与检查点 ────────────────────────────────────────────────

// Checkpoint 执行 PRAGMA wal_checkpoint(TRUNCATE)（DESIGN.md §14 第 5 条）。
//
// 不截断的话 WAL 会一路涨到几百 MB。TRUNCATE 模式会等所有读者结束，
// 因此只在写 goroutine 里调用。
func (d *DB) Checkpoint() error {
	if _, err := d.w.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("store: wal_checkpoint(TRUNCATE): %w", err)
	}
	return nil
}

// ── 句柄访问 ────────────────────────────────────────────────────

// Writer 返回单写句柄。只有 store.Writer goroutine 应当使用它。
func (d *DB) Writer() *sql.DB { return d.w }

// Reader 返回只读句柄，供查询路径使用。
func (d *DB) Reader() *sql.DB { return d.r }

// Path 是数据库文件路径；Dir 是数据目录（blobs/ 与标记文件都在这里）。
func (d *DB) Path() string { return d.path }
func (d *DB) Dir() string  { return d.dir }

// Close 关闭两个句柄，并在标记启用时删除 clean_shutdown 标记。
func (d *DB) Close() error {
	var err error
	d.closeOnce.Do(func() {
		if d.markerEnabled {
			if rmErr := os.Remove(d.markerPath()); rmErr != nil && !os.IsNotExist(rmErr) {
				err = fmt.Errorf("store: remove clean_shutdown marker: %w", rmErr)
			}
		}
		if d.r != nil {
			if cerr := d.r.Close(); cerr != nil && err == nil {
				err = cerr
			}
		}
		if d.w != nil {
			if cerr := d.w.Close(); cerr != nil && err == nil {
				err = cerr
			}
		}
	})
	return err
}

func (d *DB) execOne(query string) error {
	if _, err := d.w.Exec(query); err != nil {
		return fmt.Errorf("store: %s: %w", query, err)
	}
	return nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
