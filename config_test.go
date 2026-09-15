package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestLoadBootstrap_WritesTemplateWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.toml")

	cfg, got, err := LoadBootstrap(path)
	if err != nil {
		t.Fatalf("LoadBootstrap: %v", err)
	}
	if got != path {
		t.Errorf("返回路径 = %q，想要 %q", got, path)
	}
	if cfg.UI.Language != "system" || cfg.Log.Level != "info" || cfg.Database.Path != "" {
		t.Errorf("默认值不对：%+v", cfg)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("首次读取应当生成模板文件：%v", err)
	}
	// 模板必须能被自己解析回来
	var round BootstrapConfig
	if _, err := toml.Decode(string(data), &round); err != nil {
		t.Fatalf("生成的模板解析失败：%v", err)
	}
	if round.UI.Language != "system" {
		t.Errorf("模板里的 language = %q，想要 system", round.UI.Language)
	}
	if !strings.Contains(string(data), "settings 表") {
		t.Error("模板应当说明「其余设置存在 settings 表」这件事（§9.0）")
	}
}

func TestLoadBootstrap_ReadsExistingAndNormalizes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[database]
path = "  /tmp/custom.db  "

[ui]
language = "  zh-CN "

[log]
level = " DEBUG "
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadBootstrap(path)
	if err != nil {
		t.Fatalf("LoadBootstrap: %v", err)
	}
	if cfg.Database.Path != "/tmp/custom.db" {
		t.Errorf("database.path = %q，两边空白应被去掉", cfg.Database.Path)
	}
	if cfg.UI.Language != "zh-CN" {
		t.Errorf("ui.language = %q", cfg.UI.Language)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level = %q，应当小写化", cfg.Log.Level)
	}
}

func TestLoadBootstrap_EmptyValuesFallBackToDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[ui]\nlanguage = \"\"\n[log]\nlevel = \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadBootstrap(path)
	if err != nil {
		t.Fatalf("LoadBootstrap: %v", err)
	}
	if cfg.UI.Language != "system" {
		t.Errorf("空 language 应回落到 system，得到 %q", cfg.UI.Language)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("空 level 应回落到 info，得到 %q", cfg.Log.Level)
	}
}

func TestLoadBootstrap_BadTOMLIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("this is = = not toml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadBootstrap(path); err == nil {
		t.Fatal("坏配置应当返回错误，而不是静默用默认值（否则用户改错了都不知道）")
	}
}

func TestParseLogLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":    slog.LevelDebug,
		"DEBUG":    slog.LevelDebug,
		"info":     slog.LevelInfo,
		"":         slog.LevelInfo,
		"warn":     slog.LevelWarn,
		"warning":  slog.LevelWarn,
		"error":    slog.LevelError,
		"nonsense": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := parseLogLevel(in); got != want {
			t.Errorf("parseLogLevel(%q) = %v，想要 %v", in, got, want)
		}
	}
}

func TestDefaultBootstrapIsUsable(t *testing.T) {
	c := DefaultBootstrap()
	if c.UI.Language == "" || c.Log.Level == "" {
		t.Fatalf("默认值不该留空：%+v", c)
	}
	// 默认路径必须能解析出一个绝对路径
	p, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath: %v", err)
	}
	if !filepath.IsAbs(p) {
		t.Errorf("DefaultConfigPath = %q，应当是绝对路径", p)
	}
}
