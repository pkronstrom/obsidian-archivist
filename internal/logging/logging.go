// Package logging sets up the logger.
//
// On rotation: in a container you almost certainly do NOT want the file option.
// Docker's json-file driver already rotates (max-size, max-file) and journald
// rotates for systemd units, so logging to stderr and letting the supervisor
// handle it avoids two systems rotating the same stream. The file option exists
// for the case with neither -- a bare binary run by hand or from cron.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

func Level(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q (debug, info, warn, error)", s)
}

// New returns a logger. path == "" logs to stderr only.
func New(level slog.Level, path string, maxBytes int64, keep int) (*slog.Logger, io.Closer, error) {
	if path == "" {
		h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
		return slog.New(h), nopCloser{}, nil
	}
	w, err := newRotator(path, maxBytes, keep)
	if err != nil {
		return nil, nil, err
	}
	// Both destinations: a file to keep, and stderr so `docker logs` still
	// shows something. Silently losing `docker logs` because someone set a log
	// file would be a bad surprise.
	h := slog.NewTextHandler(io.MultiWriter(os.Stderr, w), &slog.HandlerOptions{Level: level})
	return slog.New(h), w, nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// rotator is a size-triggered rotating file writer. Deliberately tiny and
// dependency-free: rotate on size, keep N old files, nothing clever. Anything
// more belongs to the supervisor.
type rotator struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	size     int64
	f        *os.File
}

func newRotator(path string, maxBytes int64, keep int) (*rotator, error) {
	if maxBytes <= 0 {
		maxBytes = 10 << 20
	}
	if keep < 0 {
		keep = 0
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	r := &rotator{path: path, maxBytes: maxBytes, keep: keep}
	return r, r.open()
}

func (r *rotator) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, fi.Size()
	return nil
}

func (r *rotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate shifts v.log -> v.log.1 -> v.log.2 ... and drops the oldest.
func (r *rotator) rotate() error {
	if err := r.f.Close(); err != nil {
		return err
	}
	if r.keep == 0 {
		os.Remove(r.path)
		return r.open()
	}
	os.Remove(fmt.Sprintf("%s.%d", r.path, r.keep))
	for i := r.keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
	}
	if err := os.Rename(r.path, r.path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return r.open()
}

func (r *rotator) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
