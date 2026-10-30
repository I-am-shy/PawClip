//go:build windows

package main

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestAcquireSingleInstance 直接验证命名互斥量的三态：
// 第一次拿到 → 第二次 already → 释放后第三次又能拿到。
func TestAcquireSingleInstance(t *testing.T) {
	key := fmt.Sprintf("test-%d-%d", os.Getpid(), time.Now().UnixNano())

	release, already, err := acquireSingleInstance(key)
	if err != nil {
		t.Fatalf("第一次 acquire: %v", err)
	}
	if already {
		t.Fatal("第一次 acquire 不应是 already")
	}
	if release == nil {
		t.Fatal("第一次 acquire 没有返回 release")
	}

	release2, already2, err := acquireSingleInstance(key)
	if err != nil {
		t.Fatalf("第二次 acquire: %v", err)
	}
	if !already2 {
		t.Fatal("第二次 acquire 应当报告 already")
	}
	if release2 != nil {
		t.Fatal("already 分支不应返回 release（不能替对方释放）")
	}

	release()

	release3, already3, err := acquireSingleInstance(key)
	if err != nil {
		t.Fatalf("释放后 acquire: %v", err)
	}
	if already3 {
		t.Fatal("释放后应当能重新拿到")
	}
	release3()
}
