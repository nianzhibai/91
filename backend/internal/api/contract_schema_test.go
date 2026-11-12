package api

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/frontend"
)

// Check the independently maintained wire definitions against the actual DTOs.
// This catches renamed fields, omitted fields, and changed optionality or types
// without adding a runtime schema library or a second set of model definitions.
func TestOpenAPIMatchesClientDTOs(t *testing.T) {
	var document struct {
		Components struct {
			Schemas map[string]contractSchema `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openAPIDocument, &document); err != nil {
		t.Fatal(err)
	}
	for name, dto := range map[string]any{
		"VideoList": VideoListDTO{}, "VideoDTO": VideoDTO{}, "VideoCardDTO": VideoCardDTO{},
		"VideoDetailDTO": VideoDetailDTO{}, "videoFeedResponse": videoFeedResponse{},
		"shortsFeedResponse": shortsFeedResponse{}, "VideoCollectionDTO": VideoCollectionDTO{},
		"PlaybackManifestDTO": PlaybackManifestDTO{}, "createVideoShareResponse": createVideoShareResponse{},
		"Frontend": frontend.Frontend{}, "FrontendList": FrontendList{},
		"FrontendUpdate": frontend.Update{}, "FrontendUpdates": FrontendUpdates{},
	} {
		t.Run(name, func(t *testing.T) {
			schema, ok := document.Components.Schemas[name]
			if !ok {
				t.Fatalf("missing schema %s", name)
			}
			checkContractType(t, name, reflect.TypeOf(dto), schema, document.Components.Schemas)
		})
	}
}

type contractSchema struct {
	Ref        string                    `json:"$ref"`
	Type       any                       `json:"type"`
	Properties map[string]contractSchema `json:"properties"`
	Required   []string                  `json:"required"`
	Items      *contractSchema           `json:"items"`
	AllOf      []contractSchema          `json:"allOf"`
}

func resolveContractSchema(t *testing.T, schema contractSchema, schemas map[string]contractSchema) contractSchema {
	t.Helper()
	if schema.Ref != "" {
		referenced, ok := schemas[strings.TrimPrefix(schema.Ref, "#/components/schemas/")]
		if !ok {
			t.Fatalf("unresolved reference %s", schema.Ref)
		}
		return resolveContractSchema(t, referenced, schemas)
	}
	if len(schema.AllOf) > 0 {
		combined := contractSchema{Type: "object", Properties: map[string]contractSchema{}}
		for _, part := range schema.AllOf {
			part = resolveContractSchema(t, part, schemas)
			for name, field := range part.Properties {
				combined.Properties[name] = field
			}
			combined.Required = append(combined.Required, part.Required...)
		}
		return combined
	}
	return schema
}

func checkContractType(t *testing.T, path string, typ reflect.Type, schema contractSchema, schemas map[string]contractSchema) {
	t.Helper()
	schema = resolveContractSchema(t, schema, schemas)
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	wantType := ""
	switch typ.Kind() {
	case reflect.String:
		wantType = "string"
	case reflect.Bool:
		wantType = "boolean"
	case reflect.Int, reflect.Int64, reflect.Uint64:
		wantType = "integer"
	case reflect.Float64:
		wantType = "number"
	case reflect.Slice:
		wantType = "array"
		if schema.Items == nil {
			t.Fatalf("%s has no array item schema", path)
		}
		checkContractType(t, path+"[]", typ.Elem(), *schema.Items, schemas)
	case reflect.Struct:
		wantType = "object"
		if typ == reflect.TypeOf(time.Time{}) {
			wantType = "string"
			break
		}
		fields := contractJSONFields(typ)
		if len(fields) != len(schema.Properties) {
			t.Errorf("%s: DTO has %d fields, schema has %d", path, len(fields), len(schema.Properties))
		}
		for name, field := range fields {
			property, ok := schema.Properties[name]
			if !ok {
				t.Errorf("%s.%s is undocumented", path, name)
				continue
			}
			optional := slices.Contains(strings.Split(field.Tag.Get("json"), ","), "omitempty")
			if slices.Contains(schema.Required, name) == optional {
				t.Errorf("%s.%s: required disagrees with json tag %q", path, name, field.Tag.Get("json"))
			}
			checkContractType(t, path+"."+name, field.Type, property, schemas)
		}
	default:
		t.Fatalf("add wire-type checking for %s (%s)", path, typ)
	}
	types := []any{schema.Type}
	if values, ok := schema.Type.([]any); ok {
		types = values
	}
	if !slices.Contains(types, any(wantType)) {
		t.Errorf("%s: schema type %v does not match %s", path, schema.Type, wantType)
	}
}

func contractJSONFields(typ reflect.Type) map[string]reflect.StructField {
	fields := map[string]reflect.StructField{}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Anonymous {
			for name, embedded := range contractJSONFields(field.Type) {
				fields[name] = embedded
			}
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if field.IsExported() && name != "-" && name != "" {
			fields[name] = field
		}
	}
	return fields
}
