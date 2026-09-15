package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// settings 表是运行时设置的唯一真源（DESIGN.md §9.0）。
//
// 只有三项"打开数据库之前就必须读到"的引导配置留在 config.toml：
// database.path / ui.language / log.level。UI 改设置即写库，不回写 TOML，
// 因此不存在双写。

// 设置键。取值与默认值见 DESIGN.md §9 的表。
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

	KeyStorageCleanShutdownMarker = "storage.cleanShutdownMarker"
	KeyStorageWALCheckpointEvery  = "storage.walCheckpointEvery"

	KeyBackupManifestFormat = "backup.manifestFormat"
	KeyBackupIncludeExpired = "backup.includeExpired"
)

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
}

// DefaultSettings 是 DESIGN.md §9 的默认值表。默认值只在这一个地方写死。
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
		},
		Storage: StorageSettings{
			CleanShutdownMarker: true,
			WALCheckpointEvery:  1000,
		},
		Backup: BackupSettings{
			ManifestFormat: ManifestJSON,
			IncludeExpired: false,
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
// 之后 settings 表说了算，也不回写 TOML（DESIGN.md §9.0 的"不存在双写"）。
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
