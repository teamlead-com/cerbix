package store

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

var tenantReferenceBehavioralOwners = map[string]func(*testing.T){
	"status-page-component-binding":       TestACreateRefusesABindingOutsideThePagesProject,
	"monitor-notification-channel":        TestAlertRoutingSchemaRejectsDirectForeignProjectWrites,
	"escalation-policy-target":            TestAlertRoutingStoreRejectsForeignProjectReferences,
	"oncall-schedule-participant":         TestAlertRoutingStoreRejectsForeignProjectReferences,
	"oncall-override-channel":             TestAlertRoutingSchemaRejectsDirectForeignProjectWrites,
	"incident-escalation-snapshot-target": TestEscalationSnapshotRejectsForeignProjectTargets,
}

func TestTenantReferenceOwnerInventoryResolves(t *testing.T) {
	t.Parallel()

	specPath := filepath.Join("..", "..", "docs", "specs", "cross-contract-conformance.md")
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read owner inventory: %v", err)
	}
	entries := tenantReferenceEntries(string(data))
	wantKeys := tenantReferenceKeys(tenantReferenceBehavioralOwners)
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

func TestTenantReferenceOwnerBehavioralBindings(t *testing.T) {
	for _, key := range tenantReferenceKeys(tenantReferenceBehavioralOwners) {
		t.Run(key, tenantReferenceBehavioralOwners[key])
	}
}

func tenantReferenceKeys(values map[string]func(*testing.T)) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestTenantReferenceFieldsRequireRegisteredOwners(t *testing.T) {
	registered := make(map[string]bool, len(tenantReferenceBehavioralOwners))
	for key := range tenantReferenceBehavioralOwners {
		registered[key] = true
	}

	types := []reflect.Type{
		reflect.TypeOf(domain.Component{}),
		reflect.TypeOf(domain.EscalationTarget{}),
		reflect.TypeOf(domain.OnCallSchedule{}),
		reflect.TypeOf(domain.OnCallOverride{}),
		reflect.TypeOf(monitorChannelReference{}),
	}
	exemptIdentityOrScope := map[string]map[string]bool{
		"Component":               {"ID": true, "StatusPageID": true, "OrgID": true},
		"EscalationTarget":        {},
		"OnCallSchedule":          {"ID": true, "ProjectID": true},
		"OnCallOverride":          {"ID": true},
		"monitorChannelReference": {},
	}
	covered := make(map[string]bool, len(registered))
	for _, typ := range types {
		for index := 0; index < typ.NumField(); index++ {
			field := typ.Field(index)
			owners := strings.Split(field.Tag.Get("tenantref"), ",")
			if owners[0] == "" {
				owners = nil
			}
			if strings.HasSuffix(field.Name, "ID") && !exemptIdentityOrScope[typ.Name()][field.Name] && len(owners) == 0 {
				t.Errorf("%s.%s is a cross-object id field without a tenantref owner", typ.Name(), field.Name)
			}
			for _, owner := range owners {
				if !registered[owner] {
					t.Errorf("%s.%s names unregistered tenantref owner %q", typ.Name(), field.Name, owner)
					continue
				}
				covered[owner] = true
			}
		}
	}
	for key := range registered {
		if !covered[key] {
			t.Errorf("tenant-reference owner %q has no tagged field", key)
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
