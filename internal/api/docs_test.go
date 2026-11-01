package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/blind-postman/blind-postman/internal/docs"
)

// Спецификация написана вручную, поэтому тесты ловят расхождения с кодом: метод без описания,
// описание без метода и поля, которых нет с одной из сторон.

type specDoc struct {
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		Schemas map[string]struct {
			Properties map[string]any `yaml:"properties"`
		} `yaml:"schemas"`
	} `yaml:"components"`
}

func parseSpec(t *testing.T) specDoc {
	t.Helper()
	var doc specDoc
	if err := yaml.Unmarshal(docs.Spec(), &doc); err != nil {
		t.Fatalf("openapi.yaml does not parse: %v", err)
	}
	return doc
}

func TestSpecCoversEveryRoute(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(newFakeStore(), fakeBot{}, Config{}, log)

	var routes []string
	for _, r := range srv.routes() {
		routes = append(routes, r.pattern)
	}
	var described []string
	for path, ops := range parseSpec(t).Paths {
		for method := range ops {
			described = append(described, strings.ToUpper(method)+" "+path)
		}
	}
	sort.Strings(routes)
	sort.Strings(described)
	if !reflect.DeepEqual(routes, described) {
		t.Errorf("routes and openapi.yaml differ:\nroutes: %v\nspec:   %v", routes, described)
	}
}

func TestSpecSchemasMatchTypes(t *testing.T) {
	schemas := parseSpec(t).Components.Schemas
	for name, v := range map[string]any{
		"SendMessageRequest":  sendMessageRequest{},
		"SendMessageResponse": sendMessageResponse{},
		"Message":             messageDTO{},
		"CreateLinkRequest":   createLinkRequest{},
		"CreateLinkResponse":  createLinkResponse{},
		"Subscriber":          subscriberDTO{},
	} {
		schema, ok := schemas[name]
		if !ok {
			t.Errorf("schema %s is not described", name)
			continue
		}
		var described []string
		for field := range schema.Properties {
			described = append(described, field)
		}
		sort.Strings(described)
		if fields := jsonFields(v); !reflect.DeepEqual(fields, described) {
			t.Errorf("schema %s differs:\ntype: %v\nspec: %v", name, fields, described)
		}
	}
}

func jsonFields(v any) []string {
	typ := reflect.TypeOf(v)
	fields := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		fields = append(fields, name)
	}
	sort.Strings(fields)
	return fields
}

func TestDocPageNeedsNoToken(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(newFakeStore()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/doc/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "SwaggerUIBundle") {
		t.Errorf("GET /doc/: status %d", rec.Code)
	}
}
