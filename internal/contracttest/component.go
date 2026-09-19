// Package contracttest owns behavioural case inventories executed by more than one implementation.
package contracttest

type ComponentRef string

const (
	RefNone       ComponentRef = ""
	RefLocalA     ComponentRef = "local-a"
	RefLocalB     ComponentRef = "local-b"
	RefForeignOrg ComponentRef = "foreign-org"
	RefMissing    ComponentRef = "missing"
)

type ComponentPage string

const (
	PageOrg      ComponentPage = "org"
	PageProjectA ComponentPage = "project-a"
	PageMissing  ComponentPage = "missing"
)

type ComponentError string

const (
	ComponentOK               ComponentError = ""
	ComponentNotFound         ComponentError = "not-found"
	ComponentBindingNotFound  ComponentError = "binding-not-found"
	ComponentConversionTarget ComponentError = "conversion-target"
)

type ComponentCreateCase struct {
	Name              string
	Page              ComponentPage
	Monitor           ComponentRef
	Service           ComponentRef
	WantSource        string
	WantSourceProject ComponentRef
	WantError         ComponentError
}

// ComponentCreateCases is the closed case inventory shared by the API fake and PostgreSQL store.
// A backend supplies concrete fixture ids, but it may not select a smaller behavioural set.
var ComponentCreateCases = []ComponentCreateCase{
	{Name: "manual", Page: PageOrg, WantSource: "manual"},
	{Name: "monitor", Page: PageOrg, Monitor: RefLocalA, WantSource: "monitor", WantSourceProject: RefLocalA},
	{Name: "service", Page: PageOrg, Service: RefLocalA, WantSource: "service", WantSourceProject: RefLocalA},
	{Name: "service wins retained same-project pair", Page: PageOrg, Monitor: RefLocalA, Service: RefLocalA, WantSource: "service", WantSourceProject: RefLocalA},
	{Name: "missing page", Page: PageMissing, WantError: ComponentNotFound},
	{Name: "missing monitor", Page: PageOrg, Monitor: RefMissing, WantError: ComponentBindingNotFound},
	{Name: "missing service", Page: PageOrg, Service: RefMissing, WantError: ComponentBindingNotFound},
	{Name: "foreign organization", Page: PageOrg, Service: RefForeignOrg, WantError: ComponentBindingNotFound},
	{Name: "page project mismatch", Page: PageProjectA, Service: RefLocalB, WantError: ComponentConversionTarget},
	{Name: "retained pair project mismatch", Page: PageOrg, Monitor: RefLocalA, Service: RefLocalB, WantError: ComponentConversionTarget},
}
