package store

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestTenantReferenceOwnerInventoryResolves(t *testing.T) {
	t.Parallel()

	specPath := filepath.Join("..", "..", "docs", "specs", "cross-contract-conformance.md")
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read owner inventory: %v", err)
	}
	entries := tenantReferenceEntries(string(data))
	wantKeys := []string{
		"escalation-policy-target",
		"incident-escalation-snapshot-target",
		"monitor-notification-channel",
		"oncall-override-channel",
		"oncall-schedule-participant",
		"status-page-component-binding",
	}
	gotKeys := make([]string, 0, len(entries))
	for key := range entries {
		gotKeys = append(gotKeys, key)
	}
	sort.Strings(gotKeys)
	if strings.Join(gotKeys, "\n") != strings.Join(wantKeys, "\n") {
		t.Fatalf("tenant-reference key set drifted:\n got %v\nwant %v", gotKeys, wantKeys)
	}

	for key, owners := range entries {
		for _, owner := range owners {
			path, token, ok := strings.Cut(owner, "::")
			if !ok || path == "" || token == "" {
				t.Fatalf("%s owner %q must be path::token", key, owner)
			}
			body, readErr := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(path)))
			if readErr != nil {
				t.Fatalf("%s owner %q: %v", key, owner, readErr)
			}
			if !strings.Contains(string(body), token) {
				t.Fatalf("%s owner %q does not resolve", key, owner)
			}
		}
	}
}

func tenantReferenceEntries(spec string) map[string][]string {
	entries := map[string][]string{}
	inTable := false
	for _, line := range strings.Split(spec, "\n") {
		if line == "## 5. Tenant-reference owner inventory" {
			inTable = true
			continue
		}
		if inTable && strings.HasPrefix(line, "## ") {
			break
		}
		if !inTable || !strings.HasPrefix(line, "| `") {
			continue
		}
		columns := strings.Split(line, "|")
		if len(columns) != 8 {
			continue
		}
		key := markdownCode(columns[1])
		entries[key] = []string{
			markdownCode(columns[3]),
			markdownCode(columns[4]),
			markdownCode(columns[5]),
			markdownCode(columns[6]),
		}
	}
	return entries
}

func TestTenantReferenceInventoryParserDoesNotInventRows(t *testing.T) {
	if entries := tenantReferenceEntries("## 5. Tenant-reference owner inventory\n\n| Key | Reference |\n| --- | --- |\n"); len(entries) != 0 {
		t.Fatalf("empty table produced entries: %v", entries)
	}
}

func markdownCode(value string) string {
	return strings.Trim(strings.TrimSpace(value), "`")
}
