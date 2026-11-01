package docs

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSpecRefsResolve(t *testing.T) {
	var doc map[string]any
	if err := yaml.Unmarshal(Spec(), &doc); err != nil {
		t.Fatalf("openapi.yaml does not parse: %v", err)
	}

	var walk func(node any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			if ref, ok := v["$ref"].(string); ok {
				checkRef(t, doc, ref)
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
}

func checkRef(t *testing.T, doc map[string]any, ref string) {
	t.Helper()
	path, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		t.Errorf("external $ref %q", ref)
		return
	}
	node := any(doc)
	for _, key := range strings.Split(path, "/") {
		m, isMap := node.(map[string]any)
		if !isMap {
			t.Errorf("$ref %q: %q is not an object", ref, key)
			return
		}
		if node, ok = m[key]; !ok {
			t.Errorf("$ref %q does not resolve: no %q", ref, key)
			return
		}
	}
}

func TestHandler(t *testing.T) {
	srv := httptest.NewServer(Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})))
	defer srv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	get := func(path string) (*http.Response, string) {
		t.Helper()
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp, string(body)
	}

	cases := []struct {
		path        string
		status      int
		contentType string
		contains    string
	}{
		{"/doc/", http.StatusOK, "text/html", "SwaggerUIBundle"},
		{"/doc/openapi.yaml", http.StatusOK, "application/yaml", "openapi: 3.0.3"},
		{"/doc/swagger-ui/swagger-ui.css", http.StatusOK, "text/css", ".swagger-ui"},
		{"/doc/swagger-ui/swagger-ui-bundle.js", http.StatusOK, "javascript", ""},
		{"/doc/swagger-ui/favicon-32x32.png", http.StatusOK, "image/png", ""},
		{"/doc/swagger-ui/LICENSE", http.StatusOK, "", "Apache License"},
		{"/doc/nothing", http.StatusNotFound, "", ""},
		{"/doc/swagger-ui/", http.StatusNotFound, "", ""}, // без списка файлов
		{"/doc/swagger-ui/../docs.go", http.StatusNotFound, "", ""},
		{"/document", http.StatusTeapot, "", ""},
		{"/api/v1/messages", http.StatusTeapot, "", ""},
	}
	for _, c := range cases {
		resp, body := get(c.path)
		if resp.StatusCode != c.status {
			t.Errorf("GET %s: status %d, want %d", c.path, resp.StatusCode, c.status)
		}
		if !strings.Contains(resp.Header.Get("Content-Type"), c.contentType) {
			t.Errorf("GET %s: Content-Type %q, want %q", c.path, resp.Header.Get("Content-Type"), c.contentType)
		}
		if !strings.Contains(body, c.contains) {
			t.Errorf("GET %s: body does not contain %q", c.path, c.contains)
		}
	}

	if resp, _ := get("/doc"); resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/doc/" {
		t.Errorf("GET /doc: %d, Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp, err := http.Post(srv.URL+"/doc/openapi.yaml", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /doc/openapi.yaml: status %d", resp.StatusCode)
	}
}
