package api

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type openAPIContractDocument struct {
	Components struct {
		Schemas map[string]struct {
			Properties map[string]yaml.Node `yaml:"properties"`
		} `yaml:"schemas"`
	} `yaml:"components"`
}

func TestRegisteredStatusPageRequestContractsMatchOpenAPI(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var document openAPIContractDocument
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}

	contracts := []struct {
		schema string
		body   any
	}{
		{schema: "CreateStatusPage", body: createStatusPageRequest{}},
		{schema: "UpdateStatusPage", body: updateStatusPageRequest{}},
		{schema: "CreateComponent", body: createComponentRequest{}},
		{schema: "UpdateComponent", body: updateComponentRequest{}},
		{schema: "ConversionTarget", body: conversionTargetBody{}},
	}

	for _, contract := range contracts {
		t.Run(contract.schema, func(t *testing.T) {
			schema, ok := document.Components.Schemas[contract.schema]
			if !ok {
				t.Fatalf("OpenAPI schema %q is not defined", contract.schema)
			}
			want := sortedKeys(schema.Properties)
			got := jsonFields(reflect.TypeOf(contract.body))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("JSON fields drifted: Go=%v OpenAPI=%v", got, want)
			}
		})
	}
}

func jsonFields(typ reflect.Type) []string {
	fields := make([]string, 0, typ.NumField())
	for index := 0; index < typ.NumField(); index++ {
		name := strings.Split(typ.Field(index).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		fields = append(fields, name)
	}
	sort.Strings(fields)
	return fields
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
