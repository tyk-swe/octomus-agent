// Package dashboard contains the real SvelteKit production output. `all:` is
// essential: the default embed rules omit underscore-prefixed _app assets.
package dashboard

import (
	"embed"
	"io/fs"
)

// A missing entrypoint or JS bundle must fail compilation, even if an unrelated
// file remains under build. There is no development placeholder in this binary.
//
//go:embed all:build build/200.html build/_app/immutable/entry/*.js
var content embed.FS

// Files exposes read-only files rooted at the dashboard, independent of cwd.
func Files() fs.FS {
	files, err := fs.Sub(content, "build")
	if err != nil {
		panic(err)
	}
	return files
}
