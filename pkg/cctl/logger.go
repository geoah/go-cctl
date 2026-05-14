package cctl

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	logRotateBytes = 10 * 1024 * 1024 // 10MB
	logRotateKeep  = 3                // keep .log + .log.1..3
)

var (
	logLevel   slog.LevelVar
	logger     *slog.Logger
	loggerOnce sync.Once
	logPath    string
)

// initLogger opens ~/.cctl.log (rotating it if oversize) and installs the
// global slog logger. Safe to call multiple times; only the first call wires
// the file. After init, callers can update the level with setLogLevel.
func initLogger() {
	loggerOnce.Do(func() {
		home, err := os.UserHomeDir()
		if err != nil {
			logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			return
		}
		logPath = filepath.Join(home, ".cctl.log")
		rotateLog(logPath)
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			return
		}
		logLevel.Set(slog.LevelInfo)
		h := slog.NewTextHandler(f, &slog.HandlerOptions{Level: &logLevel})
		logger = slog.New(h)
		logger.Info("cctl-start", "pid", os.Getpid(), "log", logPath)
	})
}

// setLogLevel reconfigures the active level. Called after config load so the
// startup line is captured regardless of the user's preference.
func setLogLevel(s string) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		logLevel.Set(slog.LevelDebug)
	case "warn", "warning":
		logLevel.Set(slog.LevelWarn)
	case "error":
		logLevel.Set(slog.LevelError)
	case "", "info":
		logLevel.Set(slog.LevelInfo)
	default:
		log().Warn("unknown log_level — using info", "given", s)
		logLevel.Set(slog.LevelInfo)
	}
}

// log returns the global logger, falling back to a no-op if initLogger hasn't
// been called yet (so library code can call log() unconditionally).
func log() *slog.Logger {
	if logger == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return logger
}

// rotateLog shifts log → log.1 → log.2 → log.3 if the current log is too
// large. Older files past logRotateKeep are dropped. Cheap to call at startup;
// no-op when the file is small or missing.
func rotateLog(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() < logRotateBytes {
		return
	}
	// drop the oldest, then shift each backup up by one.
	oldest := fmt.Sprintf("%s.%d", path, logRotateKeep)
	_ = os.Remove(oldest)
	for i := logRotateKeep - 1; i >= 1; i-- {
		_ = os.Rename(
			fmt.Sprintf("%s.%d", path, i),
			fmt.Sprintf("%s.%d", path, i+1),
		)
	}
	_ = os.Rename(path, path+".1")
}

// abbrev shortens long strings for log fields (commands, stdout). Keeps the
// head so the meaningful prefix is visible in logs.
func abbrev(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
