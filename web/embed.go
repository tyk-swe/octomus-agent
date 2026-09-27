package dashboard

import (
	"embed"
	"io/fs"
)

//go:embed all:build build/200.html build/_app/immutable/entry/*.js
var content embed.FS

func Files() fs.FS {
	files, err := fs.Sub(content, "build")
	if err != nil {
		panic(err)
	}
	return files
}
