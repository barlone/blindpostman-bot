// Package docs раздаёт документацию API: спецификацию OpenAPI (openapi.yaml) и Swagger UI
// (swagger-ui/, swagger-ui-dist 5.34.1, Apache 2.0, LICENSE и NOTICE рядом). Всё встроено в
// бинарник, с CDN ничего не загружается.
//
// Документация открыта без токена. Методы API она только описывает, а вызвать их из
// Swagger UI можно лишь с api_token.
package docs

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

// Prefix — адрес документации.
const Prefix = "/doc"

//go:embed openapi.yaml index.html
var pages embed.FS

//go:embed swagger-ui/swagger-ui.css swagger-ui/swagger-ui-bundle.js swagger-ui/swagger-ui-standalone-preset.js
//go:embed swagger-ui/favicon-32x32.png swagger-ui/favicon-16x16.png
//go:embed swagger-ui/LICENSE swagger-ui/NOTICE swagger-ui/swagger-ui-bundle.js.LICENSE.txt
var assets embed.FS

// Spec возвращает спецификацию OpenAPI.
func Spec() []byte {
	spec, err := pages.ReadFile("openapi.yaml")
	if err != nil {
		panic(err) // файл встроен при сборке
	}
	return spec
}

// Handler раздаёт страницу Swagger UI по /doc/, спецификацию по /doc/openapi.yaml и файлы
// интерфейса под /doc/swagger-ui/.
func Handler() http.Handler {
	spec := Spec()
	started := time.Now()

	index, err := pages.ReadFile("index.html")
	if err != nil {
		panic(err) // файл встроен при сборке
	}
	ui, err := fs.Sub(assets, "swagger-ui")
	if err != nil {
		panic(err) // файл встроен при сборке
	}
	files := http.StripPrefix(Prefix+"/swagger-ui/", http.FileServer(http.FS(ui)))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		switch rest := strings.TrimPrefix(r.URL.Path, Prefix); rest {
		case "":
			// Относительные ссылки страницы работают только от адреса со слешем.
			http.Redirect(w, r, Prefix+"/", http.StatusMovedPermanently)
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			http.ServeContent(w, r, "index.html", started, bytes.NewReader(index))
		case "/openapi.yaml":
			// В таблице типов Go нет расширения .yaml.
			w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
			http.ServeContent(w, r, "openapi.yaml", started, bytes.NewReader(spec))
		default:
			if strings.HasPrefix(rest, "/swagger-ui/") && !strings.HasSuffix(rest, "/") {
				files.ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
		}
	})
}

// Wrap ставит документацию перед обработчиком API: запросы к Prefix обслуживает Handler,
// остальные — next. Поэтому они не попадают в http.TimeoutHandler, который держал бы в
// памяти весь файл интерфейса.
func Wrap(next http.Handler) http.Handler {
	doc := Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IsDocPath(r.URL.Path) {
			doc.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// IsDocPath сообщает, относится ли путь к документации: /doc или всё, что под /doc/.
// Путь вроде /document к ней не относится.
func IsDocPath(path string) bool {
	return path == Prefix || strings.HasPrefix(path, Prefix+"/")
}
