package debuglog

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"
)

var (
	cfgMu      sync.RWMutex
	cfgLog     string
	cfgLogSet  bool
	cfgSess    string
	cfgSessSet bool

	writeMu       sync.Mutex
	logFile       *os.File
	logPathCached string
)

// Configure applies runtime debug log settings from merged config.
func Configure(logPath, sessionID string) {
	cfgMu.Lock()
	cfgLog = logPath
	cfgLogSet = true
	cfgSess = sessionID
	cfgSessSet = true
	cfgMu.Unlock()

	// Close open files without holding cfgMu (lock order: never nest writeMu under cfgMu).
	writeMu.Lock()
	defer writeMu.Unlock()
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
	logPathCached = ""
	if responseFile != nil {
		_ = responseFile.Close()
		responseFile = nil
	}
	responsePathCached = ""
}

func logPath() string {
	cfgMu.RLock()
	if cfgLogSet {
		p := cfgLog
		cfgMu.RUnlock()
		return p
	}
	cfgMu.RUnlock()
	// Unit tests often inherit a developer's GOGEN_DEBUG_LOG and would
	// otherwise pollute the real session log with fixture entries.
	if testing.Testing() {
		return ""
	}
	return os.Getenv("GOGEN_DEBUG_LOG")
}

func sessionID() string {
	cfgMu.RLock()
	if cfgSessSet {
		s := cfgSess
		cfgMu.RUnlock()
		return s
	}
	cfgMu.RUnlock()
	if testing.Testing() {
		return ""
	}
	return os.Getenv("GOGEN_DEBUG_SESSION")
}

// Write appends a JSON debug entry when debug logging is configured.
func Write(location, message, hypothesisID string, data map[string]interface{}) {
	logPath := logPath()
	if logPath == "" {
		return
	}
	entry := map[string]interface{}{
		"timestamp":    time.Now().UnixMilli(),
		"location":     location,
		"message":      message,
		"hypothesisId": hypothesisID,
		"data":         data,
	}
	if sid := sessionID(); sid != "" {
		entry["sessionId"] = sid
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	line := append(b, '\n')

	writeMu.Lock()
	defer writeMu.Unlock()
	if logPath != logPathCached {
		if logFile != nil {
			_ = logFile.Close()
			logFile = nil
		}
		logPathCached = logPath
	}
	if logFile == nil {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return
		}
		logFile = f
	}
	_, _ = logFile.Write(line)
}

// CloseLog closes the debug log file (cross-platform test cleanup).
func CloseLog() {
	writeMu.Lock()
	defer writeMu.Unlock()
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
	logPathCached = ""
}
