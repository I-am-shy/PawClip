package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// settings 表是运行时设置的唯一真源（docs/DESIGN.md §9.0）。
//
// 只有三项"打开数据库之前就必须读到"的引导配置留在 config.toml：
// database.path / ui.language / log.level。UI 改设置即写库，不回写 TOML，
// 因此不存在双写。

// 设置键。取值与默认值见 docs/DESIGN.md §9 的表。
const (
	KeyCaptureEnabled              = "capture.enabled"
	KeyCaptureTypes                = "capture.types"
	KeyCaptureImageMaxBytes        = "capture.imageMaxBytes"
	KeyCaptureTextMaxChars         = "capture.textMaxChars"
	KeyCapturePollIntervalActiveMs = "capture.pollIntervalActiveMs"
	KeyCapturePollIntervalIdleMs   = "capture.pollIntervalIdleMs"
	KeyCaptureIdleThresholdSec     = "capture.idleThresholdSec"
	KeyCaptureDebounceMs           = "capture.debounceMs"

	KeyExcludeApps         = "exclude.apps"
	KeyExcludePrivateTypes = "exclude.privateTypes"

	KeyRetentionDefaultTTLSec = "retention.defaultTtlSec"
	KeyRetentionOnExpire      = "retention.onExpire"
	KeyRetentionTrashTTLSec   = "retention.trashTtlSec"
	KeyRetentionMaxItems      = "retention.maxItems"
	KeyRetentionMaxDiskBytes  = "retention.maxDiskBytes"
	KeyRetentionGCIntervalSec = "retention.gcIntervalSec"

	KeyUIHotkey               = "ui.hotkey"
	KeyUIPasteMode            = "ui.pasteMode"
	KeyUIRestoreClipboard     = "ui.restoreClipboard"
	KeyUIRestoreDelayMs       = "ui.restoreDelayMs"
	KeyUIWindowIdleDestroySec = "ui.windowIdleDestroySec"
	KeyUIQuickPasteCount      = "ui.quickPasteCount"
	KeyUILanguage             = "ui.language"
	KeyUITheme                = "ui.theme"
	KeyUICloseOnBlur          = "ui.closeOnBlur"
	KeyUIPanelWidth           = "ui.panelWidth"
	KeyUIPanelHeight          = "ui.panelHeight"
	KeyUILastView             = "ui.lastView"
	KeyUILastDraftId          = "ui.lastDraftId"
	KeyUIDraftTOCCollapsed    = "ui.draftTocCollapsed"

	KeyDraftAutoSaveDebounceMs = "draft.autoSaveDebounceMs"
	KeyDraftImageMaxBytes      = "draft.imageMaxBytes"
	KeyDraftArchiveTtlSec      = "draft.archiveTtlSec"

	KeyStorageCleanShutdownMarker = "storage.cleanShutdownMarker"
	KeyStorageWALCheckpointEvery  = "storage.walCheckpointEvery"

	KeyBackupManifestFormat = "backup.manifestFormat"
	KeyBackupIncludeExpired = "backup.includeExpired"
)

// 视图名（ui.lastView 的取值域）。与 frontend/src/App.tsx 的 View 联合一一对应。
//
// 之所以在 Go 侧也定义一遍而不是存任意字符串：这个值会被用来决定
// "下次启动进哪个页面"，写进去一个前端不认识的名字就等于启动后
// 停在空白页。有常量 + normalize，坏值只可能退化成默认的历史页。
const (
	ViewPanel    = "panel"
	ViewDrafts   = "drafts"
	ViewStats    = "stats"
	ViewBackup   = "backup"
	ViewSettings = "settings"
)

// NormalizeLastView 把 ui.lastView 收敛到已知取值。
//
// 未知值一律落回历史页（ViewPanel）——**不是**落回原值：
// 一个改过名的旧视图名会让首屏停在一个不存在的页面上。
func NormalizeLastView(v string) string {
	switch v {
	case ViewPanel, ViewDrafts, ViewStats, ViewBackup, ViewSettings:
		return v
	default:
		return ViewPanel
	}
}

// 设置值的取值域常量。
const (
	PasteModeClipboard = "clipboard"
	PasteModeAutoPaste = "autoPaste"

	OnExpireTrash   = "trash"
	OnExpireDelete  = "delete"
	OnExpireArchive = "archive"

	ManifestJSON = "json"
	ManifestYAML = "yaml"

	LanguageSystem = "system"
	ThemeSystem    = "system"
)

// CaptureSettings ← capture.*
type CaptureSettings struct {
	Enabled              bool     `json:"enabled"`
	Types                []string `json:"types"`
	ImageMaxBytes        int64    `json:"imageMaxBytes"`
	TextMaxChars         int      `json:"textMaxChars"`
	PollIntervalActiveMs int      `json:"pollIntervalActiveMs"`
	PollIntervalIdleMs   int      `json:"pollIntervalIdleMs"`
	IdleThresholdSec     int      `json:"idleThresholdSec"`
	DebounceMs           int      `json:"debounceMs"`
}

// ExcludeSettings ← exclude.*
type ExcludeSettings struct {
	Apps         []string `json:"apps"`
	PrivateTypes bool     `json:"privateTypes"`
}

// RetentionSettings ← retention.*
type RetentionSettings struct {
	DefaultTTLSec int64  `json:"defaultTtlSec"`
	OnExpire      string `json:"onExpire"`
	TrashTTLSec   int64  `json:"trashTtlSec"`
	MaxItems      int    `json:"maxItems"`
	MaxDiskBytes  int64  `json:"maxDiskBytes"`
	GCIntervalSec int    `json:"gcIntervalSec"`
}

// UISettings ← ui.*
type UISettings struct {
	// Hotkey 默认 CmdOrCtrl+Shift+V。注意：全局注册会吞掉所有 App 里的 ⌘⇧V
	// （"粘贴并匹配样式"），设置界面必须显式提示，或改用不冲突的组合。
	Hotkey               string `json:"hotkey"`
	PasteMode            string `json:"pasteMode"`
	RestoreClipboard     bool   `json:"restoreClipboard"`
	RestoreDelayMs       int    `json:"restoreDelayMs"`
	WindowIdleDestroySec int    `json:"windowIdleDestroySec"`
	QuickPasteCount      int    `json:"quickPasteCount"`
	Language             string `json:"language"`
	Theme                string `json:"theme"`

	// CloseOnBlur 为真时，用户点到面板以外的任何地方（别的 App、桌面、
	// 别的窗口）面板自动收起。
	//
	// 判据是**面板丢掉了键盘焦点**，不是"App 失去激活"：面板是
	// NonactivatingPanel，呼出时它自己拿键盘却不会把 App 切到前台
	// （docs/DESIGN.md §0.2），所以 App 层面的激活状态在这里没有意义。
	CloseOnBlur bool `json:"closeOnBlur"`

	// PanelWidth / PanelHeight 是面板尺寸（逻辑点）。
	//
	// 它们不是"给用户填的参数"，而是**用户拖出来的结果**：面板边缘可拉伸，
	// 收起面板与退出时把当前 frame 写回这里，下次启动照原样打开。
	// 取值域由 panel.ClampPanelSize 在生产侧夹取（store 不认识 panel 包，
	// 也不想认识——这里只存整数）。
	PanelWidth  int `json:"panelWidth"`
	PanelHeight int `json:"panelHeight"`

	// LastView 是"上次停在哪一页"（取值域见 ViewXxx 常量）。
	//
	// ⚠️ 这条是**改动过的既有决策**（2026-09-21）：原先的策略是"每次呼出
	// 都回到剪贴板历史"，理由是"按热键时用户要的是看看我刚复制的东西"。
	// 现在的规则换成：**只有草稿本粘滞**，其余页面（历史/统计/导出导入/设置）
	// 一律回落到历史。理由见 docs/DESIGN.md §4.4 末段——草稿是"正在写的
	// 东西"，把它换掉等于把用户的工作台收走；而设置页不是。
	LastView string `json:"lastView"`

	// DraftTOCCollapsed 是草稿本目录（目录树）的收起状态。
	//
	// 它不是交互偏好：面板默认宽 560、硬上限 760（panel.ClampPanelSize），
	// 560 下"左目录 180 + 右编辑"只剩 380 逻辑点，富文本排版放不下。
	// 所以这个开关是**布局必需**，值要活过这次运行。
	DraftTOCCollapsed bool `json:"draftTocCollapsed"`

	// LastDraftID 是"上次打开的草稿"（ui.lastView 的草稿粒度）。
	//
	// 0 表示没有记录。它只在"重新进入草稿本"时用来回到上次看的那条，
	// 不参与任何业务判断；指向的草稿可能已被删除，读取方必须校验存在性。
	LastDraftID int64 `json:"lastDraftId"`
}

// DraftSettings ← draft.*
//
// 草稿本自己的两个参数。刻意**不复用** capture.imageMaxBytes：
// 那个值的语义是"自动捕获时超过多大就不记"，用户为了减少噪音把它调小
// （比如 1 MB）是个合理的意图，而草稿贴图是用户主动放进来的内容，
// 不该跟着一起被拒。同一类参数被两件不同的事共用，改动一处就会
// 在另一处造成说不通的后果。
type DraftSettings struct {
	// AutoSaveDebounceMs 是实时保存的防抖窗口。
	AutoSaveDebounceMs int `json:"autoSaveDebounceMs"`
	// ImageMaxBytes 是单张草稿贴图的大小上限。
	ImageMaxBytes int64 `json:"imageMaxBytes"`
	// ArchiveTTLSec 是归档草稿的保留期：软删除进归档区后，超过这个
	// 时长由 GC 硬删（docs/DESIGN.md §5.4 第 3 步）。
	//
	// **刻意不与 retention.trashTtlSec 共用一个值**：剪贴板回收站里的
	// 是"自动捕获的流水"，草稿归档区里的是"用户手写的作品"，两者对
	// "留多久才敢真删"的心理预期不同——回收站 7 天够了，草稿通常要
	// 更长（默认 30 天）。共用一个键的话，用户把回收站调短，草稿
	// 会跟着被悄悄清空。
	ArchiveTTLSec int64 `json:"archiveTtlSec"`
}

// StorageSettings ← storage.*
type StorageSettings struct {
	CleanShutdownMarker bool `json:"cleanShutdownMarker"`
	WALCheckpointEvery  int  `json:"walCheckpointEvery"`
}

// BackupSettings ← backup.*
type BackupSettings struct {
	ManifestFormat string `json:"manifestFormat"`
	IncludeExpired bool   `json:"includeExpired"`
}

// Settings 是全部运行时设置的强类型视图。
type Settings struct {
	Capture   CaptureSettings   `json:"capture"`
	Exclude   ExcludeSettings   `json:"exclude"`
	Retention RetentionSettings `json:"retention"`
	UI        UISettings        `json:"ui"`
	Storage   StorageSettings   `json:"storage"`
	Backup    BackupSettings    `json:"backup"`
	Draft     DraftSettings     `json:"draft"`
}

// DefaultSettings 是 docs/DESIGN.md §9 的默认值表。默认值只在这一个地方写死。
func DefaultSettings() *Settings {
	return &Settings{
		Capture: CaptureSettings{
			Enabled:              true,
			Types:                []string{"text", "image", "files"},
			ImageMaxBytes:        10 * 1024 * 1024,
			TextMaxChars:         262144,
			PollIntervalActiveMs: 200,
			PollIntervalIdleMs:   1000,
			IdleThresholdSec:     60,
			DebounceMs:           120,
		},
		Exclude: ExcludeSettings{
			Apps:         []string{"com.1password.*", "com.apple.keychainaccess", "com.agilebits.*"},
			PrivateTypes: true,
		},
		Retention: RetentionSettings{
			DefaultTTLSec: 2592000,
			OnExpire:      OnExpireTrash,
			TrashTTLSec:   604800,
			MaxItems:      2000,
			MaxDiskBytes:  524288000,
			GCIntervalSec: 60,
		},
		UI: UISettings{
			Hotkey:               "CmdOrCtrl+Shift+V",
			PasteMode:            PasteModeClipboard,
			RestoreClipboard:     true,
			RestoreDelayMs:       250,
			WindowIdleDestroySec: 300,
			QuickPasteCount:      9,
			Language:             LanguageSystem,
			Theme:                ThemeSystem,
			CloseOnBlur:          true,
			// 560×760：420×520 太窄，分类树 + 列表一起放不下（用户实测反馈）。
			// 这个值只在"用户从没拖过边缘"时生效，一旦拖过就以库里的为准。
			PanelWidth:  560,
			PanelHeight: 760,
			// 首次启动进历史页；之后由使用过程写回。
			LastView:          ViewPanel,
			DraftTOCCollapsed: false,
		},
		Storage: StorageSettings{
			CleanShutdownMarker: true,
			WALCheckpointEvery:  1000,
		},
		Backup: BackupSettings{
			ManifestFormat: ManifestJSON,
			IncludeExpired: false,
		},
		Draft: DraftSettings{
			// 500ms 是"停手就存"与"别把写连接占满"之间的折中：
			// 低于 300ms 时逐句输入会打成一串写事务（store 是单写串行，
			// 捕获线程也在排同一条队），高于 1s 则断电丢掉的篇幅明显变多。
			// 前端另有一条 5s 的强制落盘兜住"一直在打字"的情况。
			AutoSaveDebounceMs: 500,
			ImageMaxBytes:      10 * 1024 * 1024,
			// 30 天，长于回收站的 7 天：见 ArchiveTTLSec 的字段注释。
			ArchiveTTLSec: 30 * 86400,
		},
	}
}

// binding 把设置键与结构体字段绑起来。用表驱动而不是反射，
// 是为了让"哪个 key 落在哪个字段"在代码里一眼可查。
type binding struct {
	key string
	ptr any
}

func (s *Settings) bindings() []binding {
	return []binding{
		{KeyCaptureEnabled, &s.Capture.Enabled},
		{KeyCaptureTypes, &s.Capture.Types},
		{KeyCaptureImageMaxBytes, &s.Capture.ImageMaxBytes},
		{KeyCaptureTextMaxChars, &s.Capture.TextMaxChars},
		{KeyCapturePollIntervalActiveMs, &s.Capture.PollIntervalActiveMs},
		{KeyCapturePollIntervalIdleMs, &s.Capture.PollIntervalIdleMs},
		{KeyCaptureIdleThresholdSec, &s.Capture.IdleThresholdSec},
		{KeyCaptureDebounceMs, &s.Capture.DebounceMs},

		{KeyExcludeApps, &s.Exclude.Apps},
		{KeyExcludePrivateTypes, &s.Exclude.PrivateTypes},

		{KeyRetentionDefaultTTLSec, &s.Retention.DefaultTTLSec},
		{KeyRetentionOnExpire, &s.Retention.OnExpire},
		{KeyRetentionTrashTTLSec, &s.Retention.TrashTTLSec},
		{KeyRetentionMaxItems, &s.Retention.MaxItems},
		{KeyRetentionMaxDiskBytes, &s.Retention.MaxDiskBytes},
		{KeyRetentionGCIntervalSec, &s.Retention.GCIntervalSec},

		{KeyUIHotkey, &s.UI.Hotkey},
		{KeyUIPasteMode, &s.UI.PasteMode},
		{KeyUIRestoreClipboard, &s.UI.RestoreClipboard},
		{KeyUIRestoreDelayMs, &s.UI.RestoreDelayMs},
		{KeyUIWindowIdleDestroySec, &s.UI.WindowIdleDestroySec},
		{KeyUIQuickPasteCount, &s.UI.QuickPasteCount},
		{KeyUILanguage, &s.UI.Language},
		{KeyUITheme, &s.UI.Theme},
		{KeyUICloseOnBlur, &s.UI.CloseOnBlur},
		{KeyUIPanelWidth, &s.UI.PanelWidth},
		{KeyUIPanelHeight, &s.UI.PanelHeight},
		{KeyUILastView, &s.UI.LastView},
		{KeyUILastDraftId, &s.UI.LastDraftID},
		{KeyUIDraftTOCCollapsed, &s.UI.DraftTOCCollapsed},

		{KeyDraftAutoSaveDebounceMs, &s.Draft.AutoSaveDebounceMs},
		{KeyDraftImageMaxBytes, &s.Draft.ImageMaxBytes},
		{KeyDraftArchiveTtlSec, &s.Draft.ArchiveTTLSec},

		{KeyStorageCleanShutdownMarker, &s.Storage.CleanShutdownMarker},
		{KeyStorageWALCheckpointEvery, &s.Storage.WALCheckpointEvery},

		{KeyBackupManifestFormat, &s.Backup.ManifestFormat},
		{KeyBackupIncludeExpired, &s.Backup.IncludeExpired},
	}
}

// SeedSettings 把缺失的设置键补成默认值。
//
// bootstrapLanguage 来自 config.toml 的 ui.language：它只在"库还没建"的时候
// 有意义，所以仅在 ui.language 这一行确实不存在时作为初始值用一次；
// 之后 settings 表说了算，也不回写 TOML（docs/DESIGN.md §9.0 的"不存在双写"）。
func SeedSettings(ctx context.Context, d *DB, bootstrapLanguage string) error {
	def := DefaultSettings()
	if bootstrapLanguage != "" {
		def.UI.Language = bootstrapLanguage
	}
	existing, err := d.AllRaw(ctx)
	if err != nil {
		return err
	}
	for _, b := range def.bindings() {
		if _, ok := existing[b.key]; ok {
			continue
		}
		raw, err := json.Marshal(b.ptr)
		if err != nil {
			return fmt.Errorf("store: encode default for %s: %w", b.key, err)
		}
		if err := d.SetRaw(ctx, b.key, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

// LoadSettings 读出全部设置。缺失的键保持默认值，JSON 解不开的键保留默认值
// 并跳过（一个坏行不应该让应用起不来）。
func LoadSettings(ctx context.Context, d *DB) (*Settings, error) {
	s := DefaultSettings()
	if err := LoadSettingsInto(ctx, d, s); err != nil {
		return nil, err
	}
	return s, nil
}

// LoadSettingsInto 把库里的值覆盖到给定 Settings 上（便于测试与热重载）。
func LoadSettingsInto(ctx context.Context, d *DB, s *Settings) error {
	rows, err := d.AllRaw(ctx)
	if err != nil {
		return err
	}
	var badKeys []string
	for _, b := range s.bindings() {
		raw, ok := rows[b.key]
		if !ok {
			continue
		}
		if err := json.Unmarshal([]byte(raw), b.ptr); err != nil {
			badKeys = append(badKeys, b.key)
		}
	}
	if len(badKeys) > 0 {
		d.log.Warn("some settings rows are not valid JSON; using defaults for them",
			"keys", badKeys)
	}
	// 视图名收敛在**读完的第一时间**做，而不是在用它的时候：
	// 它决定下次启动进哪一页，坏值（改过名的旧视图、手改库写进去的乱串）
	// 必须在离开这个函数之前就变成已知取值。
	s.UI.LastView = NormalizeLastView(s.UI.LastView)
	return nil
}

// SaveSettings 写回全部设置键。
func SaveSettings(ctx context.Context, d *DB, s *Settings) error {
	for _, b := range s.bindings() {
		raw, err := json.Marshal(b.ptr)
		if err != nil {
			return fmt.Errorf("store: encode %s: %w", b.key, err)
		}
		if err := d.SetRaw(ctx, b.key, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

// SettingKeys 返回全部受管设置键（不含 category / tag 之类的业务数据）。
func SettingKeys() []string {
	keys := make([]string, 0, 32)
	for _, b := range DefaultSettings().bindings() {
		keys = append(keys, b.key)
	}
	return keys
}

// ── 键值层 ──────────────────────────────────────────────────────

// GetRaw 读一个设置的原始 JSON 值。
func (d *DB) GetRaw(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := d.r.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	switch {
	case err == nil:
		return v, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	default:
		return "", false, fmt.Errorf("store: read setting %s: %w", key, err)
	}
}

// SetRaw 写一个设置的原始 JSON 值。走单写句柄，保证与捕获写入同一串行化纪律。
func (d *DB) SetRaw(ctx context.Context, key, value string) error {
	_, err := d.w.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("store: write setting %s: %w", key, err)
	}
	return nil
}

// AllRaw 读回全部设置。
func (d *DB) AllRaw(ctx context.Context) (map[string]string, error) {
	rows, err := d.r.QueryContext(ctx, "SELECT key, value FROM settings")
	if err != nil {
		return nil, fmt.Errorf("store: read settings: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string, 32)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: scan setting: %w", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate settings: %w", err)
	}
	return out, nil
}
