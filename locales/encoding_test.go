package locales

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A catalog written through a shell with the wrong code page turns every non-ASCII letter into "?".
// Such a string still parses and passes the key-parity checks, so this looks at the text itself:
// the bot catalogs, the console strings and the answer language packs.
func TestCatalogTextSurvivedItsEncoding(t *testing.T) {
	files := map[string][]byte{}
	for _, fsys := range []fs.FS{FS, UIFS} {
		_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".json") {
				files[p], _ = fs.ReadFile(fsys, p)
			}
			return nil
		})
	}
	packs, _ := filepath.Glob(filepath.Join("..", "internal", "lang", "packs", "*.json"))
	for _, p := range packs {
		files[p], _ = os.ReadFile(p)
	}
	if len(files) < 15 {
		t.Fatalf("found only %d catalogs", len(files))
	}
	for name, raw := range files {
		var walk func(path string, v any)
		walk = func(path string, v any) {
			switch x := v.(type) {
			case string:
				if strings.Contains(x, "??") || strings.ContainsRune(x, '�') {
					t.Errorf("%s %s looks mangled by a wrong encoding: %q", name, path, x)
				}
			case map[string]any:
				for k, e := range x {
					walk(path+"."+k, e)
				}
			case []any:
				for _, e := range x {
					walk(path+"[]", e)
				}
			}
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		walk("", v)
	}
}
