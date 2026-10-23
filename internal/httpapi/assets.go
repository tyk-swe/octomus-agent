package httpapi

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	dashboard "github.com/tyk-swe/octomus-agent/web"
)

const indexName = "200.html"

const immutablePrefix = "_app/immutable/"

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

func compressible(name string) bool {
	switch path.Ext(name) {
	case ".js", ".css", ".html", ".svg", ".json":
		return true
	}
	return false
}

func acceptsGzip(r *http.Request) bool {
	for _, token := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		coding, parameters, _ := strings.Cut(token, ";")
		if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
			continue
		}
		acceptable := true
		for _, parameter := range strings.Split(parameters, ";") {
			key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if ok && strings.EqualFold(strings.TrimSpace(key), "q") {
				weight, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
				acceptable = err == nil && weight > 0
			}
		}
		if acceptable {
			return true
		}
	}
	return false
}

type embeddedAssets struct {
	files fs.FS
	gzip  sync.Map
}

func (e *embeddedAssets) compressed(name string, data []byte) []byte {
	if cached, ok := e.gzip.Load(name); ok {
		return cached.([]byte)
	}
	var buffer bytes.Buffer
	writer, _ := gzip.NewWriterLevel(&buffer, gzip.BestCompression)
	_, _ = writer.Write(data)
	_ = writer.Close()
	cached, _ := e.gzip.LoadOrStore(name, buffer.Bytes())
	return cached.([]byte)
}

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
	if strings.HasPrefix(name, immutablePrefix) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	if compressible(name) {
		w.Header().Add("Vary", "Accept-Encoding")
		if acceptsGzip(r) {
			if compressed := e.compressed(name, data); len(compressed) < len(data) {
				w.Header().Set("Content-Encoding", "gzip")
				serveFile(w, r, name, compressed)
				return
			}
		}
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
		if strings.HasPrefix(name, "_app/") || hasExtension(name) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
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
