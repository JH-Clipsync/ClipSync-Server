package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

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
}

// 新建一个按天切割的 writer，日志目录不存在会自动创建
func newDailyRotatingWriter(dir, prefix string) (*dailyRotatingWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建日志目录失败: %w", err)
	}
	// 归档子目录也提前建好
	if err := os.MkdirAll(filepath.Join(dir, prefix), 0o755); err != nil {
		return nil, fmt.Errorf("创建归档目录失败: %w", err)
	}
	w := &dailyRotatingWriter{
		dir:    dir,
		prefix: prefix,
	}
	if err := w.openToday(time.Now()); err != nil {
		return nil, err
	}
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
func setupLogWriter(dir, prefix string, stdout bool) (io.Writer, io.Closer, error) {
	rw, err := newDailyRotatingWriter(dir, prefix)
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
func newCategoryLogger(dir, prefix string) (*log.Logger, io.Closer, error) {
	rw, err := newDailyRotatingWriter(dir, prefix)
	if err != nil {
		return nil, nil, err
	}
	l := log.New(rw, "", log.LstdFlags|log.Lmicroseconds)
	return l, rw, nil
}
