package httpapi

import (
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	dashboard "github.com/tyk-swe/octomus-agent/web"
)

const indexName = "200.html"

const assetMethods = "GET, HEAD"

func assetHandler(override string) http.Handler {
	if override != "" {
		return &overrideAssets{root: http.Dir(override)}
	}
	return &embeddedAssets{files: dashboard.Files()}
}

func decodedPath(urlPath string) (string, bool) {
	if !utf8.ValidString(urlPath) {
		return "", false
	}
	trimmed := strings.TrimLeft(urlPath, "/")
	for _, part := range strings.Split(trimmed, "/") {
		if part == ".." || part == "." {
			return "", false
		}
	}
	if strings.ContainsAny(trimmed, "\\\x00") {
		return "", false
	}
	return trimmed, true
}

func assetName(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", assetMethods)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return "", false
	}
	name, ok := decodedPath(r.URL.Path)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return "", false
	}
	return name, true
}

func hasExtension(name string) bool {
	base := name[strings.LastIndex(name, "/")+1:]
	i := strings.LastIndex(base, ".")
	return i > 0
}

func serveFile(w http.ResponseWriter, r *http.Request, name string, contents []byte) {
	w.Header().Set("Content-Type", contentType(name))
	w.Header().Set("Content-Length", strconv.Itoa(len(contents)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(contents)
	}
}

func contentType(name string) string {
	if typ := mime.TypeByExtension(path.Ext(name)); typ != "" {
		return typ
	}
	return "application/octet-stream"
}

type embeddedAssets struct{ files fs.FS }

func (e *embeddedAssets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := assetName(w, r)
	if !ok {
		return
	}
	if name == "" {
		name = indexName
	}
	data, err := fs.ReadFile(e.files, name)
	if err != nil {
		if strings.HasPrefix(name, "_app/") || hasExtension(name) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		data, err = fs.ReadFile(e.files, indexName)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		name = indexName
	}
	serveFile(w, r, name, data)
}

type overrideAssets struct{ root http.FileSystem }

func (o *overrideAssets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := assetName(w, r)
	if !ok {
		return
	}
	data, served, found := o.read(name)
	if !found {
		data, served, found = o.read(indexName)
	}
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	serveFile(w, r, served, data)
}

func (o *overrideAssets) read(name string) (data []byte, served string, ok bool) {
	if name == "" {
		name = "index.html"
	}
	file, err := o.root.Open("/" + name)
	if err != nil {
		return nil, "", false
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, "", false
	}
	if stat.IsDir() {
		name = strings.TrimSuffix(name, "/") + "/index.html"
		index, err := o.root.Open("/" + name)
		if err != nil {
			return nil, "", false
		}
		defer index.Close()
		file = index
	}
	data, err = io.ReadAll(file)
	return data, name, err == nil
}
