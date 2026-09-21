package api

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type openAPIContractProperty struct {
	Type   string `yaml:"type"`
	Format string `yaml:"format"`
	Ref    string `yaml:"$ref"`
}

type openAPIContractSchema struct {
	Type       string                             `yaml:"type"`
	Required   []string                           `yaml:"required"`
	Properties map[string]openAPIContractProperty `yaml:"properties"`
}

type openAPIContractDocument struct {
	Components struct {
		Schemas map[string]openAPIContractSchema `yaml:"schemas"`
	} `yaml:"components"`
}

type requestContract struct {
	schema string
	body   any
}

var registeredStatusPageRequestContracts = []requestContract{
	{schema: "CreateStatusPage", body: createStatusPageRequest{}},
	{schema: "UpdateStatusPage", body: updateStatusPageRequest{}},
	{schema: "CreateComponent", body: createComponentRequest{}},
	{schema: "UpdateComponent", body: updateComponentRequest{}},
	{schema: "ConversionTarget", body: conversionTargetBody{}},
}

func TestRegisteredStatusPageRequestContractsMatchOpenAPI(t *testing.T) {
	t.Parallel()

	document := readOpenAPIContractDocument(t)
	for _, contract := range registeredStatusPageRequestContracts {
		t.Run(contract.schema, func(t *testing.T) {
			schema, ok := document.Components.Schemas[contract.schema]
			if !ok {
				t.Fatalf("OpenAPI schema %q is not defined", contract.schema)
			}
			if drift := requestContractDrift(document, schema, reflect.TypeOf(contract.body)); len(drift) != 0 {
				t.Fatalf("request contract drifted:\n- %s", strings.Join(drift, "\n- "))
			}
		})
	}
}

func TestRequestContractGuardDetectsRequiredTypeAndFormatDrift(t *testing.T) {
	t.Parallel()

	document := readOpenAPIContractDocument(t)
	base := document.Components.Schemas["CreateStatusPage"]
	typ := reflect.TypeOf(createStatusPageRequest{})

	for _, tc := range []struct {
		name   string
		mutate func(*openAPIContractSchema)
		want   string
	}{
		{
			name: "required",
			mutate: func(schema *openAPIContractSchema) {
				schema.Required = []string{"title"}
			},
			want: "required fields drifted",
		},
		{
			name: "type",
			mutate: func(schema *openAPIContractSchema) {
				property := schema.Properties["slug"]
				property.Type = "integer"
				schema.Properties["slug"] = property
			},
			want: `field "slug" type drifted`,
		},
		{
			name: "format",
			mutate: func(schema *openAPIContractSchema) {
				property := schema.Properties["project_id"]
				property.Format = "date-time"
				schema.Properties["project_id"] = property
			},
			want: `field "project_id" format drifted`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := cloneContractSchema(base)
			tc.mutate(&schema)
			drift := strings.Join(requestContractDrift(document, schema, typ), "\n")
			if !strings.Contains(drift, tc.want) {
				t.Fatalf("drift = %q, want %q", drift, tc.want)
			}
		})
	}
}

func readOpenAPIContractDocument(t *testing.T) openAPIContractDocument {
	t.Helper()
	data, err := os.ReadFile("../../openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var document openAPIContractDocument
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	return document
}

func requestContractDrift(document openAPIContractDocument, schema openAPIContractSchema, typ reflect.Type) []string {
	goFields, goRequired := goRequestContract(typ)
	drift := make([]string, 0)

	gotNames := sortedKeys(goFields)
	wantNames := sortedKeys(schema.Properties)
	if !reflect.DeepEqual(gotNames, wantNames) {
		drift = append(drift, fmt.Sprintf("JSON fields drifted: Go=%v OpenAPI=%v", gotNames, wantNames))
	}

	sort.Strings(goRequired)
	openAPIRequired := append([]string(nil), schema.Required...)
	sort.Strings(openAPIRequired)
	if !equalStrings(goRequired, openAPIRequired) {
		drift = append(drift, fmt.Sprintf("required fields drifted: Go=%v OpenAPI=%v", goRequired, openAPIRequired))
	}

	for _, name := range gotNames {
		goField := goFields[name]
		property, ok := schema.Properties[name]
		if !ok {
			continue
		}
		openAPIType, err := effectiveOpenAPIType(document, property)
		if err != nil {
			drift = append(drift, fmt.Sprintf("field %q: %v", name, err))
			continue
		}
		if goField.typ != openAPIType {
			drift = append(drift, fmt.Sprintf("field %q type drifted: Go=%s OpenAPI=%s", name, goField.typ, openAPIType))
		}
		if goField.format != property.Format {
			drift = append(drift, fmt.Sprintf("field %q format drifted: Go=%q OpenAPI=%q", name, goField.format, property.Format))
		}
	}
	return drift
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type goContractField struct {
	typ    string
	format string
}

func goRequestContract(typ reflect.Type) (map[string]goContractField, []string) {
	fields := make(map[string]goContractField, typ.NumField())
	required := make([]string, 0)
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		fieldType := field.Type
		for fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}
		contractField := goContractField{typ: jsonSchemaType(fieldType)}
		if fieldType.Kind() == reflect.Int64 {
			contractField.format = "int64"
		}
		for _, option := range strings.Split(field.Tag.Get("contract"), ",") {
			switch {
			case option == "required":
				required = append(required, name)
			case strings.HasPrefix(option, "format="):
				contractField.format = strings.TrimPrefix(option, "format=")
			}
		}
		fields[name] = contractField
	}
	return fields, required
}

func jsonSchemaType(typ reflect.Type) string {
	switch typ.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Array, reflect.Slice:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	default:
		return typ.Kind().String()
	}
}

func effectiveOpenAPIType(document openAPIContractDocument, property openAPIContractProperty) (string, error) {
	if property.Type != "" {
		return property.Type, nil
	}
	const prefix = "#/components/schemas/"
	if !strings.HasPrefix(property.Ref, prefix) {
		return "", fmt.Errorf("has neither type nor local schema reference")
	}
	name := strings.TrimPrefix(property.Ref, prefix)
	referenced, ok := document.Components.Schemas[name]
	if !ok || referenced.Type == "" {
		return "", fmt.Errorf("references schema %q without a type", name)
	}
	return referenced.Type, nil
}

func cloneContractSchema(schema openAPIContractSchema) openAPIContractSchema {
	clone := schema
	clone.Required = append([]string(nil), schema.Required...)
	clone.Properties = make(map[string]openAPIContractProperty, len(schema.Properties))
	for name, property := range schema.Properties {
		clone.Properties[name] = property
	}
	return clone
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
