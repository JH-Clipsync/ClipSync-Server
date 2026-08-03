package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ===== 日志级别 =====
//
// 标准库 log 没有级别概念，这里用一个最小实现补上：把 cfg.Logs.Level 解析成
// 阈值，低于阈值的日志直接丢弃。只在通用日志（log.Printf）上生效；
// 消息流水日志（msgLog）是业务审计，不参与级别过滤。
type LogLevel int

const (
	LevelDebug LogLevel = iota
	LevelInfo
	LevelWarn
	LevelError
)

// parseLevel 把配置里的字符串解析成级别，无法识别时回落到 info
func parseLevel(s string) LogLevel {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

// currentLevel 是全局日志阈值，main 启动时根据配置设置。
// 默认 info，保证未初始化时行为与改造前一致。
var currentLevel = LevelInfo

// SetLogLevel 设置全局日志阈值
func SetLogLevel(l LogLevel) { currentLevel = l }

// logAt 按级别打印通用日志：低于阈值的直接丢弃。
// 保留 log.Printf 的调用手感，方便逐点替换。
func logAt(l LogLevel, format string, args ...any) {
	if l < currentLevel {
		return
	}
	log.Printf(format, args...)
}

func logDebug(format string, args ...any) { logAt(LevelDebug, format, args...) }
func logInfo(format string, args ...any)  { logAt(LevelInfo, format, args...) }
func logWarn(format string, args ...any)  { logAt(LevelWarn, format, args...) }
func logError(format string, args ...any) { logAt(LevelError, format, args...) }

// dailyRotatingWriter 是一个按日期自动切割的日志 Writer。
//
// 目录布局约定：
//
//	logs/
//	├── clipsync.log             ← 今天的通用日志（不带日期）
//	├── message.log              ← 今天的消息推送日志（不带日期）
//	├── clipsync/
//	│   ├── clipsync-2026-07-28.log   ← 归档：旧日期
//	│   └── clipsync-2026-07-29.log
//	└── message/
//	    └── message-2026-07-28.log
//
// 每次写入前会检查当前日期是否变化：若变化，先把 "<prefix>.log" 重命名为
// "<prefix>/<prefix>-YYYY-MM-DD.log"（旧日期），再打开新的 "<prefix>.log"
// 作为当天写入目标。
type dailyRotatingWriter struct {
	dir        string // 日志根目录（如 "logs"）
	prefix     string // 文件名前缀（如 "clipsync" / "message"）
	mu         sync.Mutex
	currentDay string   // 当前打开的文件对应的日期（yyyy-mm-dd）
	file       *os.File // 当前文件句柄
	maxAgeDays int      // 归档保留天数（<=0 表示不清理）
}

// 新建一个按天切割的 writer，日志目录不存在会自动创建
// maxAgeDays > 0 时，每次切割后会清理归档目录里超期的日志文件。
func newDailyRotatingWriter(dir, prefix string, maxAgeDays int) (*dailyRotatingWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建日志目录失败: %w", err)
	}
	// 归档子目录也提前建好
	if err := os.MkdirAll(filepath.Join(dir, prefix), 0o755); err != nil {
		return nil, fmt.Errorf("创建归档目录失败: %w", err)
	}
	w := &dailyRotatingWriter{
		dir:        dir,
		prefix:     prefix,
		maxAgeDays: maxAgeDays,
	}
	if err := w.openToday(time.Now()); err != nil {
		return nil, err
	}
	// 启动时先清一次：服务长期停机后重启，过期归档同样应当被回收
	w.cleanupExpired(time.Now())
	return w, nil
}

// currentPath 返回当日写入文件的完整路径（不带日期后缀）
func (w *dailyRotatingWriter) currentPath() string {
	return filepath.Join(w.dir, w.prefix+".log")
}

// archivePathFor 返回归档到子目录的路径（带日期后缀）
func (w *dailyRotatingWriter) archivePathFor(day string) string {
	return filepath.Join(w.dir, w.prefix, fmt.Sprintf("%s-%s.log", w.prefix, day))
}

// openToday 打开当天的日志文件；若已存在不带日期名的文件但其修改时间
// 属于更早的一天，则先把它归档到子目录再打开新文件。
func (w *dailyRotatingWriter) openToday(now time.Time) error {
	today := now.Format("2006-01-02")
	cur := w.currentPath()

	// 若已有当日无日期文件，判断它的日期归属：以文件的修改时间为准
	if info, err := os.Stat(cur); err == nil {
		fileDay := info.ModTime().Format("2006-01-02")
		if fileDay != today {
			// 归档为 <prefix>/<prefix>-<fileDay>.log
			if err := w.archiveFile(cur, fileDay); err != nil {
				return err
			}
		}
	}

	f, err := os.OpenFile(cur, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %w", err)
	}
	if w.file != nil {
		_ = w.file.Close()
	}
	w.file = f
	w.currentDay = today
	return nil
}

// archiveFile 把 srcPath 归档到子目录，并且如果目标已存在则合并追加
func (w *dailyRotatingWriter) archiveFile(srcPath, day string) error {
	dst := w.archivePathFor(day)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// 目标文件不存在 → 直接 rename；已存在 → 追加合并后删除源文件
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		return os.Rename(srcPath, dst)
	}
	// 已存在则做合并追加
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	// 合并完成再删除源文件
	return os.Remove(srcPath)
}

// cleanupExpired 删除归档目录里超过 maxAgeDays 天的日志。
// 日期取自文件名（<prefix>-YYYY-MM-DD.log）而不是 mtime，
// 因为归档时的 rename/合并都会刷新 mtime，按文件名判断才准。
// maxAgeDays <= 0 时直接返回，保持"不清理"的默认语义。
func (w *dailyRotatingWriter) cleanupExpired(now time.Time) {
	if w.maxAgeDays <= 0 {
		return
	}
	archiveDir := filepath.Join(w.dir, w.prefix)
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		return // 归档目录还不存在，无需清理
	}
	// 保留 maxAgeDays 天：cutoff 之前的日期全部删除
	cutoff := now.AddDate(0, 0, -w.maxAgeDays)
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		day, ok := w.dayFromArchiveName(e.Name())
		if !ok {
			continue // 不是本 prefix 的归档文件，别乱删
		}
		if day.Before(cutoff) {
			if err := os.Remove(filepath.Join(archiveDir, e.Name())); err == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		fmt.Fprintf(os.Stderr, "🧹 已清理 %s 过期归档日志 %d 个（保留 %d 天）\n",
			w.prefix, removed, w.maxAgeDays)
	}
}

// dayFromArchiveName 从 "<prefix>-YYYY-MM-DD.log" 中解出日期。
// 文件名不符合本 prefix 的归档格式时返回 ok=false。
func (w *dailyRotatingWriter) dayFromArchiveName(name string) (time.Time, bool) {
	wantPrefix := w.prefix + "-"
	if !strings.HasPrefix(name, wantPrefix) || !strings.HasSuffix(name, ".log") {
		return time.Time{}, false
	}
	dayStr := strings.TrimSuffix(strings.TrimPrefix(name, wantPrefix), ".log")
	day, err := time.Parse("2006-01-02", dayStr)
	if err != nil {
		return time.Time{}, false
	}
	return day, true
}

// Write 实现 io.Writer；写入前判断日期是否变化
func (w *dailyRotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	today := time.Now().Format("2006-01-02")
	if today != w.currentDay {
		// 关闭当前句柄，把旧文件归档到子目录，重开当天文件
		if w.file != nil {
			_ = w.file.Close()
			w.file = nil
		}
		if err := w.archiveFile(w.currentPath(), w.currentDay); err != nil {
			// 归档失败也尽量不阻断服务，只记录到 stderr
			fmt.Fprintf(os.Stderr, "⚠ 归档旧日志失败: %v\n", err)
		}
		if err := w.openToday(time.Now()); err != nil {
			return 0, err
		}
		// 刚跨天切割完，顺手回收超期归档
		w.cleanupExpired(time.Now())
	}
	return w.file.Write(p)
}

// Close 关闭当前日志文件
func (w *dailyRotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}

// setupLogWriter 构建 "写通用日志文件 + (可选) 写控制台" 的 MultiWriter。
// stdout=true 时同时输出到 stdout，便于 docker logs / kubectl logs。
// maxAgeDays 透传给 writer，用于清理超期归档。
func setupLogWriter(dir, prefix string, stdout bool, maxAgeDays int) (io.Writer, io.Closer, error) {
	rw, err := newDailyRotatingWriter(dir, prefix, maxAgeDays)
	if err != nil {
		return nil, nil, err
	}
	if stdout {
		return io.MultiWriter(os.Stdout, rw), rw, nil
	}
	return rw, rw, nil
}

// newCategoryLogger 构建一个"仅写自己那一份日志文件"的 *log.Logger，
// 用于把某一类日志（如消息推送）从通用日志中拆出来独立成文件。
// 该 logger 不写控制台，也不写通用日志，避免重复。
func newCategoryLogger(dir, prefix string, maxAgeDays int) (*log.Logger, io.Closer, error) {
	rw, err := newDailyRotatingWriter(dir, prefix, maxAgeDays)
	if err != nil {
		return nil, nil, err
	}
	l := log.New(rw, "", log.LstdFlags|log.Lmicroseconds)
	return l, rw, nil
}
