// Package logrot 服务模式的日志轮转：hearth.log 超过 10MB 改名 hearth.log.1
// （旧的 .1 顺延为 .2 …），最多保留 5 个备份。自研几十行，不引 lumberjack。
package logrot

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// Writer 单文件大小轮转 writer（io.Writer，并发安全）。
// 文件不存在则创建；已存在且已超限时先轮转再写。
type Writer struct {
	path     string
	maxBytes int64
	backups  int // 保留的备份个数（hearth.log.1 … hearth.log.N）

	mu   sync.Mutex
	f    *os.File
	size int64
}

// New path 是日志文件路径；maxBytes 触发轮转的阈值；backups 是保留备份数。
func New(path string, maxBytes int64, backups int) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	w := &Writer{path: path, maxBytes: maxBytes, backups: backups}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) open() error {
	st, err := os.Stat(w.path)
	if err == nil && st.Size() >= w.maxBytes {
		w.rotate() // 上次留下的文件已超限，先轮转
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w.f = f
	if st, err := f.Stat(); err == nil {
		w.size = st.Size()
	}
	return nil
}

// Write 写日志；写入后文件会超限时先轮转。
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size+int64(len(p)) > w.maxBytes && w.size > 0 {
		w.rotate()
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate 关闭当前文件并顺延备份：.N-1→.N（删最老）… .1→.2、当前→.1，然后重开新文件。
func (w *Writer) rotate() {
	w.f.Close()
	for i := w.backups - 1; i >= 1; i-- {
		os.Rename(w.path+"."+strconv.Itoa(i), w.path+"."+strconv.Itoa(i+1))
	}
	os.Rename(w.path, w.path+".1")
	w.f, _ = os.OpenFile(w.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	w.size = 0
}

// Close 关闭底层文件。
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
