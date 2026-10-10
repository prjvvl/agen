// Package templates holds the ready-made agent bundles (examples/bundles,
// copied in by scripts/gen.sh) that `agen init` and ListTemplates offer.
package templates

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
)

//go:embed bundles
var bundles embed.FS

// Template is one bundle of the catalog.
type Template struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Category    string `json:"category"`
	Description string `json:"description"`
	// Secrets named in the bundle's x-agen/secrets.json.
	Secrets []string `json:"-"`
	// Files maps bundle paths to their text.
	Files map[string]string `json:"-"`
}

// FS returns the files of one template, rooted at the bundle.
func FS(name string) (fs.FS, error) {
	sub, err := fs.Sub(bundles, path.Join("bundles", name))
	if err != nil {
		return nil, err
	}
	if st, err := fs.Stat(sub, "plugin.json"); err != nil || st.IsDir() {
		return nil, fmt.Errorf("no template %q", name)
	}
	return sub, nil
}

// List returns the catalog in its listed order, with every template's files.
func List() ([]Template, error) {
	raw, err := bundles.ReadFile("bundles/catalog.json")
	if err != nil {
		return nil, err
	}
	var list []Template
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("catalog.json: %w", err)
	}
	for i := range list {
		t := &list[i]
		src, err := FS(t.Name)
		if err != nil {
			return nil, err
		}
		t.Files = map[string]string{}
		err = fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := fs.ReadFile(src, p)
			t.Files[p] = string(b)
			return err
		})
		if err != nil {
			return nil, err
		}
		var secrets map[string]json.RawMessage
		_ = json.Unmarshal([]byte(t.Files["x-agen/secrets.json"]), &secrets)
		for k := range secrets {
			t.Secrets = append(t.Secrets, k)
		}
		sort.Strings(t.Secrets)
	}
	return list, nil
}

// Names returns the template names in catalog order.
func Names() []string {
	list, _ := List()
	names := make([]string, len(list))
	for i, t := range list {
		names[i] = t.Name
	}
	return names
}
