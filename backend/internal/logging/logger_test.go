package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/config"
)

func TestNewJSONLoggerUsesConfiguredLevel(t *testing.T) {
	var out bytes.Buffer
	logger := New(config.LogConfig{Level: "error", Format: "json"}, &out)
	logger.Info("hidden")
	logger.Error("visible", "monitor_id", "m1")

	got := out.String()
	if strings.Contains(got, "hidden") {
		t.Fatalf("info log emitted at error level: %s", got)
	}
	if !strings.Contains(got, `"msg":"visible"`) || !strings.Contains(got, `"monitor_id":"m1"`) {
		t.Fatalf("json log missing fields: %s", got)
	}
}

func TestCriticalLevelRendersName(t *testing.T) {
	var out bytes.Buffer
	logger := New(config.LogConfig{Level: "debug", Format: "json"}, &out)
	Critical(logger, "config_load_failed", "path", "/tmp/config.yaml")

	var entry map[string]any
	if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
		t.Fatalf("invalid critical JSON: %v: %s", err, out.String())
	}
	if entry["level"] != "CRITICAL" || entry["msg"] != "config_load_failed" {
		t.Fatalf("critical entry = %#v", entry)
	}
}

func TestTextLogger(t *testing.T) {
	var out bytes.Buffer
	logger := New(config.LogConfig{Level: "info", Format: "text"}, &out)
	logger.Info("visible", "key", "value")

	got := out.String()
	if !strings.Contains(got, "msg=visible") || !strings.Contains(got, "key=value") {
		t.Fatalf("text log missing fields: %s", got)
	}
}
