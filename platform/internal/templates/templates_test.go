package templates

import (
	"testing"

	"github.com/prjvvl/agen/platform/internal/bundle"
)

// Every template must be a valid bundle and be listed in the catalog.
func TestTemplatesAreValidBundles(t *testing.T) {
	list, err := List()
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, tpl := range list {
		listed[tpl.Name] = true
		if tpl.Title == "" || tpl.Category == "" || tpl.Description == "" {
			t.Errorf("%s: title, category and description are required in catalog.json", tpl.Name)
		}
		files := map[string][]byte{}
		for p, s := range tpl.Files {
			files[p] = []byte(s)
		}
		b, err := bundle.Parse(files)
		if err != nil {
			t.Errorf("%s: %v", tpl.Name, err)
			continue
		}
		if b.Name != tpl.Name {
			t.Errorf("%s: bundle is named %q", tpl.Name, b.Name)
		}
	}
	for _, tpl := range list {
		switch tpl.Name {
		case "hello":
			if len(tpl.Tools) != 0 || len(tpl.Skills) != 1 || tpl.Skills[0] != "greeting" {
				t.Errorf("hello: tools %v skills %v", tpl.Tools, tpl.Skills)
			}
		case "researcher":
			if len(tpl.Tools) != 1 || tpl.Tools[0] != "fetch" {
				t.Errorf("researcher: tools %v", tpl.Tools)
			}
		}
	}
	entries, err := bundles.ReadDir("bundles")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() && !listed[e.Name()] {
			t.Errorf("bundles/%s is not in catalog.json", e.Name())
		}
	}
}
