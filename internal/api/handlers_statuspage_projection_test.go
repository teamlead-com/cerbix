package api

import (
	"reflect"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func TestAffectedComponentIndex(t *testing.T) {
	components := []domain.Component{
		{ID: "first-monitor", Source: domain.ComponentSourceMonitor, MonitorID: "mon-1"},
		{ID: "first-service", Source: domain.ComponentSourceService, ServiceID: "svc-1"},
		{ID: "second-monitor", Source: domain.ComponentSourceMonitor, MonitorID: "mon-1"},
		{ID: "first-monitor", Source: domain.ComponentSourceMonitor, MonitorID: "mon-1"},
		{ID: "dormant-monitor", Source: domain.ComponentSourceService, MonitorID: "mon-1", ServiceID: "svc-2"},
		{ID: "dormant-service", Source: domain.ComponentSourceMonitor, MonitorID: "mon-2", ServiceID: "svc-1"},
	}

	index := newAffectedComponentIndex(components)
	for _, tc := range []struct {
		name string
		in   domain.Incident
		want []string
	}{
		{
			name: "monitor binding keeps page order and deduplicates",
			in:   domain.Incident{MonitorID: "mon-1"},
			want: []string{"first-monitor", "second-monitor"},
		},
		{
			name: "service binding uses only the active service source",
			in:   domain.Incident{ServiceID: "svc-1"},
			want: []string{"first-service"},
		},
		{
			name: "service anchor is canonical if malformed data has both",
			in:   domain.Incident{MonitorID: "mon-1", ServiceID: "svc-1"},
			want: []string{"first-service"},
		},
		{
			name: "project incident has a present empty relation",
			in:   domain.Incident{},
			want: []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := index.affectedComponentIDs(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("affected component ids = %#v, want %#v", got, tc.want)
			}
		})
	}
}
