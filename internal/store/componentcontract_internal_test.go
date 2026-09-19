package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/teamlead-com/cerbix/internal/contracttest"
	"github.com/teamlead-com/cerbix/internal/domain"
)

type componentContractFixture struct {
	orgID      string
	projectIDs map[contracttest.ComponentRef]string
	monitorIDs map[contracttest.ComponentRef]string
	serviceIDs map[contracttest.ComponentRef]string
	pageIDs    map[contracttest.ComponentPage]string
}

func TestPostgreSQLStoreComponentCreateContract(t *testing.T) {
	for _, contract := range contracttest.ComponentCreateCases {
		t.Run(contract.Name, func(t *testing.T) {
			st, ctx := serviceSchemaStore(t)
			fixture := seedComponentContractFixture(t, st)
			component := domain.Component{
				StatusPageID: fixture.pageIDs[contract.Page],
				Name:         "Contract component",
				MonitorID:    fixture.monitorIDs[contract.Monitor],
				ServiceID:    fixture.serviceIDs[contract.Service],
			}
			created, err := st.CreateComponent(ctx, component)
			if got := storeComponentContractError(err); got != contract.WantError {
				t.Fatalf("error class = %q, want %q: %v", got, contract.WantError, err)
			}
			if contract.WantError != contracttest.ComponentOK {
				return
			}
			if string(created.Source) != contract.WantSource {
				t.Fatalf("source = %q, want %q", created.Source, contract.WantSource)
			}
			if created.SourceProject != fixture.projectIDs[contract.WantSourceProject] {
				t.Fatalf("source project = %q, want %q", created.SourceProject, fixture.projectIDs[contract.WantSourceProject])
			}
			if created.OrgID != fixture.orgID {
				t.Fatalf("org id = %q, want %q", created.OrgID, fixture.orgID)
			}
		})
	}
}

func seedComponentContractFixture(t *testing.T, st *Store) componentContractFixture {
	t.Helper()
	ctx := t.Context()
	org, err := st.CreateOrganization(ctx, "component-contract", "Component contract")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	foreignOrg, err := st.CreateOrganization(ctx, "component-contract-foreign", "Component contract foreign")
	if err != nil {
		t.Fatalf("create foreign org: %v", err)
	}
	projectIDs := map[contracttest.ComponentRef]string{}
	for ref, owner := range map[contracttest.ComponentRef]string{
		contracttest.RefLocalA:     org.ID,
		contracttest.RefLocalB:     org.ID,
		contracttest.RefForeignOrg: foreignOrg.ID,
	} {
		project, createErr := st.CreateProject(ctx, owner, "project-"+string(ref), "Project "+string(ref))
		if createErr != nil {
			t.Fatalf("create project %s: %v", ref, createErr)
		}
		projectIDs[ref] = project.ID
	}
	monitorIDs := map[contracttest.ComponentRef]string{contracttest.RefMissing: "00000000-0000-4000-8000-000000000099"}
	serviceIDs := map[contracttest.ComponentRef]string{contracttest.RefMissing: "00000000-0000-4000-8000-000000000099"}
	for _, ref := range []contracttest.ComponentRef{contracttest.RefLocalA, contracttest.RefLocalB, contracttest.RefForeignOrg} {
		monitor, createErr := st.CreateMonitor(ctx, domain.Monitor{
			ProjectID: projectIDs[ref], Name: "Monitor " + string(ref), Type: domain.MonitorHTTP,
			Target: "https://" + string(ref) + ".example.test", IntervalSeconds: 60, Enabled: true,
		})
		if createErr != nil {
			t.Fatalf("create monitor %s: %v", ref, createErr)
		}
		monitorIDs[ref] = monitor.ID
		service, createErr := st.CreateService(ctx, domain.Service{
			ProjectID: projectIDs[ref], Slug: "service-" + string(ref), Name: "Service " + string(ref),
		})
		if createErr != nil {
			t.Fatalf("create service %s: %v", ref, createErr)
		}
		serviceIDs[ref] = service.ID
	}
	orgPage, err := st.CreateStatusPage(ctx, domain.StatusPage{
		OrgID: org.ID, Slug: "component-contract-org", Title: "Org", Visibility: domain.VisibilityInternal,
	})
	if err != nil {
		t.Fatalf("create org page: %v", err)
	}
	projectPage, err := st.CreateStatusPage(ctx, domain.StatusPage{
		OrgID: org.ID, ProjectID: projectIDs[contracttest.RefLocalA], Slug: "component-contract-project", Title: "Project", Visibility: domain.VisibilityInternal,
	})
	if err != nil {
		t.Fatalf("create project page: %v", err)
	}
	return componentContractFixture{
		orgID:      org.ID,
		projectIDs: projectIDs,
		monitorIDs: monitorIDs,
		serviceIDs: serviceIDs,
		pageIDs: map[contracttest.ComponentPage]string{
			contracttest.PageOrg:      orgPage.ID,
			contracttest.PageProjectA: projectPage.ID,
			contracttest.PageMissing:  "00000000-0000-4000-8000-000000000098",
		},
	}
}

func storeComponentContractError(err error) contracttest.ComponentError {
	switch {
	case err == nil:
		return contracttest.ComponentOK
	case errors.Is(err, ErrNotFound):
		return contracttest.ComponentNotFound
	case errors.Is(err, ErrComponentBindingNotFound):
		return contracttest.ComponentBindingNotFound
	case errors.Is(err, ErrComponentConversionTarget):
		return contracttest.ComponentConversionTarget
	default:
		return contracttest.ComponentError(fmt.Sprint(err))
	}
}
