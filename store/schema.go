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
	"net/url"
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

// 驱动名。不直接用 "sqlite3"，是为了给**只读句柄**也挂上驱动级配置
// （读句柄要开 query_only，见 dsnWith）。
const (
	driverRW = "pawclip_sqlite3_rw"
	driverRO = "pawclip_sqlite3_ro"
)

// pragmas 是每个连接都要生效的连接级 PRAGMA。
//
// ⚠️ 这里用 DSN 参数（`_foreign_keys=on` 这类）而不是 ConnectHook，
// 有两个理由，第二个是硬的：
//
//  1. busy_timeout / foreign_keys / synchronous 都是**连接级**设置。
//     数据库连接池每新开一条连接都要重新设一遍，只在第一条上设会留坑。
//     DSN 参数由驱动在每次 Open 时应用，天然满足这一点。
//
//  2. **ConnectHook 拿到的 *sqlite3.SQLiteConn 在非 cgo 构建下是个空桩**
//     （mattn/go-sqlite3 的 static_mock.go，`//go:build !cgo`，那个
//     SQLiteConn 只有 RegisterXxx 系列方法、没有 Exec）。原来这里写
//     `c.Exec(pragma, nil)`，在 CGO_ENABLED=0 交叉编译时直接编译不过：
//
//     store/schema.go:196:18: c.Exec undefined
//     (type *sqlite3.SQLiteConn has no field or method Exec)
//
//     也就是说那行代码把"能不能编译出 Windows 版"绑在了 cgo 上。
//     改成 DSN 参数之后，本包不再触碰任何驱动内部类型，跨平台编译干净。
var pragmas = []struct{ key, val string }{
	{"_foreign_keys", "on"},    // 外键级联：categories/tags 删除要连带清理
	{"_busy_timeout", "5000"},  // 锁等待 5s（DESIGN.md §4.1 的连接约定）
	{"_synchronous", "NORMAL"}, // WAL 下的推荐值
}

// dsnWith 给数据库路径拼上 PRAGMA 参数。
//
// 只读句柄额外开 query_only：让"误写"立刻报错，而不是悄悄绕开单写纪律。
// 这是设计上的一条纪律（§2 第 1 条），靠约定守不住，得靠 SQLite 自己拦。
func dsnWith(path string, readOnly bool) string {
	q := url.Values{}
	for _, p := range pragmas {
		q.Set(p.key, p.val)
	}
	if readOnly {
		q.Set("_query_only", "on")
	}
	// 路径里可能已经有查询串（例如用户传了 file:...?_txlock=immediate），
	// 那就用 & 续接；直接再拼一个 ? 会让 SQLite 把第一个 ? 之后的
	// 整段当成文件名的一部分。
	sep := "?"
	if strings.ContainsRune(path, '?') {
		sep = "&"
	}
	return path + sep + q.Encode()
}

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
	// pragmaNote 记录连接级 PRAGMA 自检的异常（空串表示全部生效）。
	pragmaNote string
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

	w, err := sql.Open(driverRW, dsnWith(opts.Path, false))
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

	ro, err := sql.Open(driverRO, dsnWith(opts.Path, true))
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

	// PRAGMA 自检：确认 DSN 参数**真的**生效了。
	//
	// 为什么要查一遍：DSN 参数只在驱动认识这个键时才生效。如果哪天
	// go-sqlite3 改了参数名（历史上 `_synchronous` 就同时接受 `_sync`），
	// 我们会得到一个"静默不设 PRAGMA"的库——尤其是 foreign_keys=OFF，
	// 它会让 ON DELETE CASCADE 全部失效，而**任何测试都不会因此变红**，
	// 只是分类/标签删除后留下悬空引用。这种失败必须被显式看见。
	if note := d.verifyPragmas(); note != "" {
		d.pragmaNote = note
		log.Error("连接级 PRAGMA 没有按预期生效，请检查 DSN 参数名是否被驱动改名",
			"detail", note)
	}

	// 迁移完成后写入标记：进程活着就代表"可能没正常退出"。
	if err := d.writeMarker(); err != nil {
		log.Warn("cannot write clean_shutdown marker", "err", err)
	}
	return d, nil
}

func registerDrivers() {
	driverOnce.Do(func() {
		// 两个驱动都**不带 ConnectHook**：PRAGMA 全走 DSN 参数（见 pragmas）。
		// 之所以还要注册两个名字而不是直接用 "sqlite3"：读句柄需要一套
		// 不同的 DSN（query_only），而 sql.Register 是按名字全局注册的，
		// 分开注册能让"哪个句柄该是什么配置"在 Open 处一眼可见。
		sql.Register(driverRW, &sqlite3.SQLiteDriver{})
		sql.Register(driverRO, &sqlite3.SQLiteDriver{})
	})
}

var driverOnce sync.Once

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
		// 这里降级是**故意**的：开发时不该因为缺标签就开不起库。
		// 但它同时也是"正式产物静默丢掉全文检索"的唯一信号，所以文案必须
		// 直接给出补救动作——实测中这行 WARN 很容易被忽略（见 scripts/build.sh）。
		d.log.Warn("FTS5 不可用，检索将退化为 LIKE；请用 scripts/build.sh 构建（等价于 wails build -tags sqlite_fts5）",
			"err", err)
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
		d.log.Warn("FTS 自检失败，检索退化为 LIKE；若为正式产物请确认构建带了 -tags sqlite_fts5",
			"err", err)
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

// PragmaNote 返回连接级 PRAGMA 自检的异常（空串表示全部按预期生效）。
func (d *DB) PragmaNote() string { return d.pragmaNote }

// verifyPragmas 逐项读回连接级 PRAGMA，确认 DSN 参数真的被驱动接受了。
//
// 为什么值得专门查一遍：DSN 参数若写错名字，**不报错、只是不生效**。
// 最要命的是 foreign_keys —— 它是 OFF 时 ON DELETE CASCADE 全部失效，
// 表现为"删掉分类后 items.category_id 指向一个不存在的分类"，
// 而所有测试仍然全绿（因为没有任何测试会去删一个分类再看子表）。
// 这类静默失效必须显式暴露。
//
// 返回空串表示全部正常。
func (d *DB) verifyPragmas() string {
	type check struct {
		pragma string
		want   string
		handle *sql.DB
	}
	checks := []check{
		{"foreign_keys", "1", d.w},
		{"busy_timeout", "5000", d.w},
		{"synchronous", "1", d.w}, // 1 = NORMAL
		{"query_only", "0", d.w},  // 写句柄必须能写
	}
	if d.r != nil {
		checks = append(checks, check{"query_only", "1", d.r})
	}

	var bad []string
	for _, c := range checks {
		var got string
		// PRAGMA 不支持参数绑定，这里的 sql 文本全部是包内常量，无注入面。
		if err := c.handle.QueryRow("PRAGMA " + c.pragma).Scan(&got); err != nil {
			bad = append(bad, fmt.Sprintf("%s: 读回失败: %v", c.pragma, err))
			continue
		}
		if got != c.want {
			bad = append(bad, fmt.Sprintf("%s = %s，期望 %s", c.pragma, got, c.want))
		}
	}
	return strings.Join(bad, "; ")
}

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
