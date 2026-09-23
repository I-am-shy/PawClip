package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// 本文件只负责**引导配置**（docs/DESIGN.md §9.0）。
//
// §9.0 定了一条硬规则：settings 表是运行时设置的唯一真源，只有三项
// "打开数据库之前就必须读到"的配置留在 config.toml：
//
//	database.path / ui.language / log.level
//
// UI 改设置即写库，**不回写 TOML**，因此不存在双写。
// 这里的读取逻辑刻意只碰这三项——多加一个字段就等于多开一条双写路径。

// BootstrapConfig 是 config.toml 的全貌。
type BootstrapConfig struct {
	Database DatabaseConfig `toml:"database"`
	UI       UIConfig       `toml:"ui"`
	Log      LogConfig      `toml:"log"`
}

// DatabaseConfig ← [database]
type DatabaseConfig struct {
	// Path 是 SQLite 文件路径。留空则按平台约定放在数据目录下。
	Path string `toml:"path"`
}

// UIConfig ← [ui]
type UIConfig struct {
	// Language 只在**首次建库**时作为 settings.ui.language 的初值用一次
	// （见 store.SeedSettings）。之后 settings 表说了算。
	// 取值：system | zh-CN | en
	Language string `toml:"language"`
}

// LogConfig ← [log]
type LogConfig struct {
	// Level: debug | info | warn | error
	Level string `toml:"level"`
}

// Version 是当前构建的版本号。
//
// 唯一真源是 wails.json 的 info.productVersion（它同时进 Info.plist 的
// CFBundleShortVersionString）—— scripts/build.sh 在构建时用 -ldflags 把它
// 注入到这里。所以下面的零值只是"直接 go run / go test 时的占位"，正常构建
// 的产物绝不会报出它：一旦看到 -dev，说明这次构建绕过了 build.sh。
//
// 两份版本号曾经是各写各的（plist 说 0.1.0、日志说 0.1.0-m1），后果是用户
// 报问题时无法确认他手上的构建，所以特意收敛成一份。
var Version = "0.1.3-dev"

// appDirName 是数据目录名（同时也是 config.toml 所在目录名）。
const appDirName = "PawClip"

// DefaultBootstrap 是 config.toml 缺失时的默认值。
func DefaultBootstrap() BootstrapConfig {
	return BootstrapConfig{
		Database: DatabaseConfig{Path: ""},
		UI:       UIConfig{Language: "system"},
		Log:      LogConfig{Level: "info"},
	}
}

// DefaultDir 返回数据目录（也是 config.toml 所在目录）。
//
// macOS:   ~/Library/Application Support/PawClip
// Windows: %AppData%\PawClip
//
// 库文件、blobs/、clean_shutdown 标记、config.toml 全在这一个目录里——
// 标记文件必须与库文件同目录（store 的约定），放一起最不容易出错。
func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", msgf(msgErrConfigDir, err, err)
	}
	return filepath.Join(base, appDirName), nil
}

// DefaultConfigPath 返回 config.toml 的默认路径。
func DefaultConfigPath() (string, error) {
	dir, err := DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

// DefaultDatabasePath 返回 SQLite 文件的默认路径。
func DefaultDatabasePath() (string, error) {
	dir, err := DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "pawclip.db"), nil
}

// LoadBootstrap 读取引导配置。
//
// path 为空时用默认路径。文件不存在时**写出**一份带注释的默认配置再返回默认值：
// 这样用户第一次跑完就能直接编辑它，而不用去猜有哪些键。
func LoadBootstrap(path string) (BootstrapConfig, string, error) {
	if strings.TrimSpace(path) == "" {
		p, err := DefaultConfigPath()
		if err != nil {
			return BootstrapConfig{}, "", err
		}
		path = p
	}

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		cfg := DefaultBootstrap()
		if werr := writeDefaultConfig(path); werr != nil {
			// 写不进去不该让应用起不来——用默认值继续跑。
			return cfg, path, nil
		}
		return cfg, path, nil
	case err != nil:
		return BootstrapConfig{}, path, msgf(msgErrConfigRead, err, path, err)
	}

	cfg := DefaultBootstrap()
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return BootstrapConfig{}, path, msgf(msgErrConfigParse, err, path, err)
	}
	return normalizeBootstrap(cfg), path, nil
}

// normalizeBootstrap 把明显非法的取值拉回默认值，而不是让应用起不来。
func normalizeBootstrap(c BootstrapConfig) BootstrapConfig {
	c.UI.Language = strings.TrimSpace(c.UI.Language)
	if c.UI.Language == "" {
		c.UI.Language = "system"
	}
	c.Log.Level = strings.TrimSpace(strings.ToLower(c.Log.Level))
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	c.Database.Path = strings.TrimSpace(c.Database.Path)
	return c
}

// writeDefaultConfig 生成一份带注释的默认 config.toml。
func writeDefaultConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(defaultConfigTOML), 0o600)
}

// defaultConfigTOML 是首次运行时生成的模板。
const defaultConfigTOML = `# PawClip 引导配置
#
# 这里**只有**三项：打开数据库之前就必须知道的东西。
# 其余所有设置（捕获类型、排除应用、保留策略、主题……）都存在数据库的
# settings 表里，由界面修改，不会回写到本文件。详见 docs/DESIGN.md §9.0。

[database]
# SQLite 文件路径。留空 = 放在平台默认数据目录：
#   macOS   ~/Library/Application Support/PawClip/pawclip.db
#   Windows %AppData%\PawClip\pawclip.db
path = ""

[ui]
# 界面语言：system | zh-CN | en
# 只在首次建库时作为初始值生效一次，之后以数据库里的设置为准。
language = "system"

[log]
# 日志级别：debug | info | warn | error
level = "info"
`

// parseLogLevel 把配置里的字符串转成 slog 级别。无法识别时退回 info。
func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// newLogger 构造应用日志器。
//
// M1 就写到 stderr：`wails dev` 会把 stderr 透到终端，够用。
// 落文件属于 M2/M3 的事（还要配轮转），现在加只是徒增体积。
func newLogger(level string) *slog.Logger {
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLogLevel(level)})
	return slog.New(h)
}
