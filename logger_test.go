package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]LogLevel{
		"debug":    LevelDebug,
		"DEBUG":    LevelDebug,
		"info":     LevelInfo,
		"warn":     LevelWarn,
		"warning":  LevelWarn,
		"error":    LevelError,
		" Error ":  LevelError,
		"":         LevelInfo, // 未配置回落 info
		"nonsense": LevelInfo, // 无法识别也回落 info
	}
	for in, want := range cases {
		if got := parseLevel(in); got != want {
			t.Errorf("parseLevel(%q) = %v, 期望 %v", in, got, want)
		}
	}
}

// 级别阈值应当过滤掉低于自身的日志：设成 error 后 info 不应写入文件
func TestLogLevelFiltersOutput(t *testing.T) {
	dir := t.TempDir()
	rw, err := newDailyRotatingWriter(dir, "lvl", 0)
	if err != nil {
		t.Fatalf("创建 writer 失败: %v", err)
	}
	defer rw.Close()

	origLevel := currentLevel
	defer SetLogLevel(origLevel)

	// 把标准库 log 指向临时文件，测完还原，避免污染其它测试
	origFlags := log.Flags()
	log.SetOutput(rw)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(origFlags)
	}()

	SetLogLevel(LevelError)
	logInfo("这条 info 不该出现")
	logError("这条 error 应该出现")

	data, err := os.ReadFile(filepath.Join(dir, "lvl.log"))
	if err != nil {
		t.Fatalf("读日志失败: %v", err)
	}
	out := string(data)
	if strings.Contains(out, "这条 info 不该出现") {
		t.Error("level=error 时 info 日志仍被写入")
	}
	if !strings.Contains(out, "这条 error 应该出现") {
		t.Error("level=error 时 error 日志未被写入")
	}
}

// max_age_days > 0 时应删除超期归档，且不碰保留期内的文件和非归档文件
func TestCleanupExpiredRemovesOldArchives(t *testing.T) {
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "clipsync")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	stale := "clipsync-" + now.AddDate(0, 0, -10).Format("2006-01-02") + ".log"
	fresh := "clipsync-" + now.AddDate(0, 0, -2).Format("2006-01-02") + ".log"
	foreign := "message-2020-01-01.log" // 别的 prefix，不该被删
	junk := "notes.txt"                 // 非日志文件，不该被删

	for _, name := range []string{stale, fresh, foreign, junk} {
		if err := os.WriteFile(filepath.Join(archiveDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	w := &dailyRotatingWriter{dir: dir, prefix: "clipsync", maxAgeDays: 7}
	w.cleanupExpired(now)

	if fileExists(filepath.Join(archiveDir, stale)) {
		t.Errorf("超期归档 %s 应被删除", stale)
	}
	for _, keep := range []string{fresh, foreign, junk} {
		if !fileExists(filepath.Join(archiveDir, keep)) {
			t.Errorf("%s 不该被删除", keep)
		}
	}
}

// max_age_days = 0 保持"不清理"语义
func TestCleanupExpiredDisabled(t *testing.T) {
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "clipsync")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(archiveDir, "clipsync-2000-01-01.log")
	if err := os.WriteFile(old, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := &dailyRotatingWriter{dir: dir, prefix: "clipsync", maxAgeDays: 0}
	w.cleanupExpired(time.Now())

	if !fileExists(old) {
		t.Error("max_age_days=0 时不应删除任何归档")
	}
}

func TestDayFromArchiveName(t *testing.T) {
	w := &dailyRotatingWriter{prefix: "clipsync"}
	if _, ok := w.dayFromArchiveName("clipsync-2026-08-03.log"); !ok {
		t.Error("合法归档名应被解析")
	}
	for _, bad := range []string{
		"message-2026-08-03.log",  // 其它 prefix
		"clipsync.log",            // 当日文件，无日期
		"clipsync-notadate.log",   // 日期非法
		"clipsync-2026-08-03.txt", // 后缀不对
	} {
		if _, ok := w.dayFromArchiveName(bad); ok {
			t.Errorf("%q 不该被当作归档文件", bad)
		}
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
