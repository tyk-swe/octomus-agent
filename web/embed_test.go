package dashboard

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedDashboardIncludesEveryProductionAsset(t *testing.T) {
	files := Files()
	entry, err := fs.ReadFile(files, "200.html")
	if err != nil || len(entry) == 0 {
		t.Fatal("missing real dashboard", err)
	}
	bundles := 0
	err = filepath.WalkDir("build", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		relative, err := filepath.Rel("build", path)
		if err != nil {
			return err
		}
		want, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		got, err := fs.ReadFile(files, filepath.ToSlash(relative))
		if err != nil {
			return err
		}
		if string(got) != string(want) {
			t.Errorf("asset changed: %s", relative)
		}
		if filepath.Ext(path) == ".js" {
			bundles++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if bundles == 0 {
		t.Fatal("underscore-prefixed _app bundles were omitted")
	}
}
