package main

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestInstanceKey_PerDatabasePath(t *testing.T) {
	dir := t.TempDir()
	a := BootstrapConfig{Database: DatabaseConfig{Path: filepath.Join(dir, "a.db")}}
	b := BootstrapConfig{Database: DatabaseConfig{Path: filepath.Join(dir, "b.db")}}

	if instanceKey(a) != instanceKey(a) {
		t.Fatal("同一路径两次 key 不一致")
	}
	if instanceKey(a) == instanceKey(b) {
		t.Fatal("不同数据库路径得到了同一个 key")
	}

	if runtime.GOOS == "windows" {
		upper := BootstrapConfig{Database: DatabaseConfig{Path: string([]rune(filepath.Join(dir, "a.db")))}}
		upper.Database.Path = upperPath(upper.Database.Path)
		if instanceKey(a) != instanceKey(upper) {
			t.Fatal("Windows 上大小写不同的同一路径应当得到同一个 key")
		}
	}
}

func upperPath(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}
