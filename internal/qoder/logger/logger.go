// Package logger is a minimal stdout logger for the ported qoder runtime.
//
// The upstream qoder2api ships a richer logger (in-memory ring + rotating file
// sink). For the embedded runtime port we only need the level-filtered
// Debug/Info/Error surface the bridge/account/checkin code calls, so this is a
// deliberate simplification of qoder2api/logger — same API subset, stdout only.
package logger

import (
	"fmt"
	"sync"
)

type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelError Level = "error"
)

var (
	mu           sync.RWMutex
	currentLevel = LevelInfo
)

// SetLevel sets the minimum level that will be emitted ("debug"|"info"|"error").
func SetLevel(level string) {
	mu.Lock()
	defer mu.Unlock()
	if level == "" {
		return
	}
	currentLevel = Level(level)
}

func shouldLog(level Level) bool {
	mu.RLock()
	defer mu.RUnlock()
	priority := map[Level]int{LevelDebug: 0, LevelInfo: 1, LevelError: 2}
	return priority[level] >= priority[currentLevel]
}

func logLine(level Level, format string, args ...interface{}) {
	if !shouldLog(level) {
		return
	}
	fmt.Printf("[qoder][%s] %s\n", level, fmt.Sprintf(format, args...))
}

func Debug(format string, args ...interface{}) { logLine(LevelDebug, format, args...) }
func Info(format string, args ...interface{})  { logLine(LevelInfo, format, args...) }
func Error(format string, args ...interface{}) { logLine(LevelError, format, args...) }
