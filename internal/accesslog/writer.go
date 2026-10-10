package accesslog

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Writer is a buffered, append-only log file. Writes go to memory and a
// background goroutine flushes them, so request goroutines never wait on
// disk. It implements io.Writer and is safe for concurrent use.
type Writer struct {
	path   string
	mu     sync.Mutex
	f      *os.File
	bw     *bufio.Writer
	stop   chan struct{}
	done   chan struct{}
	closed bool
}

// Open opens (creating if needed) path for appending, flushing every
// interval (default 1s).
func Open(path string, interval time.Duration) (*Writer, error) {
	if interval <= 0 {
		interval = time.Second
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	w := &Writer{path: path, f: f, bw: bufio.NewWriterSize(f, 256<<10), stop: make(chan struct{}), done: make(chan struct{})}
	go w.flushLoop(interval)
	return w, nil
}

func (w *Writer) flushLoop(every time.Duration) {
	defer close(w.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			w.mu.Lock()
			_ = w.bw.Flush()
			w.mu.Unlock()
		case <-w.stop:
			return
		}
	}
}

// Write buffers p.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	return w.bw.Write(p)
}

// Reopen flushes buffered lines and reopens the file by name, so a log
// rotator can move the old file aside and signal us (SIGHUP). If the file
// cannot be reopened the old one stays in use.
func (w *Writer) Reopen() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return os.ErrClosed
	}
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	ferr := w.bw.Flush()
	w.bw.Reset(f)
	w.f.Close()
	w.f = f
	return ferr
}

// Close flushes and closes the file.
func (w *Writer) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	close(w.stop)
	w.mu.Unlock()
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.bw.Flush(); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}

// Logger formats Entries and writes them to an io.Writer.
type Logger struct {
	w    io.Writer
	json bool
	pool sync.Pool
}

// NewLogger writes format ("cachelog" or "cachelog-json") lines to w.
func NewLogger(w io.Writer, format string) *Logger {
	l := &Logger{w: w, json: format == "cachelog-json"}
	l.pool.New = func() any { b := make([]byte, 0, 1024); return &b }
	return l
}

// Log formats and writes e. It never blocks on disk when w is a *Writer.
func (l *Logger) Log(e *Entry) {
	bp := l.pool.Get().(*[]byte)
	b := (*bp)[:0]
	if l.json {
		b = AppendJSON(b, e)
	} else {
		b = AppendCachelog(b, e)
	}
	_, _ = l.w.Write(b)
	*bp = b
	l.pool.Put(bp)
}
