//go:build cgo

package treesitter

import "testing"

// TestDefinitionQueriesCompile ensures every bundled definition query
// compiles for its target language. Grammar upgrades can change field
// types or alias node kinds (e.g. JavaScript aliases method-name
// identifiers to property_identifier), turning a pattern into an
// impossible one — which makes the whole query fail to compile and
// breaks ListDefinitions for that language entirely.
func TestDefinitionQueriesCompile(t *testing.T) {
	queryOnce.Do(initQueries)
	registryOnce.Do(initRegistry)
	for lang := range queryByLang {
		lang := lang
		t.Run(lang, func(t *testing.T) {
			if languageFor(lang) == nil {
				t.Fatalf("no language registered for %q", lang)
			}
			if _, err := queryForLang(lang); err != nil {
				t.Fatalf("compile query for %s: %v", lang, err)
			}
		})
	}
}

// TestListDefinitionsJavascript guards the specific regression where
// (method_definition name: (identifier)) made the whole JavaScript
// query fail to compile.
func TestListDefinitionsJavascript(t *testing.T) {
	src := []byte(`
class Greeter {
  greet(name) {
    return "hello " + name;
  }
  #secret() {}
}
function helper() {}
`[1:])
	defs, err := listDefinitions("greeter.js", src)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"Greeter": false, "greet": false, "helper": false}
	for _, d := range defs {
		if _, ok := want[d.Name]; ok {
			want[d.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("expected definition %q in %+v", name, defs)
		}
	}
}
