//go:build cgo

package treesitter

import (
	"embed"
	"fmt"
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

//go:embed queries/*.scm
var queryFS embed.FS

// queryForLang returns the compiled definition query for langName, sourced
// from the registry's defsQuery embed path and compiled through the shared
// query cache (compileQuery).
func queryForLang(langName string) (*tree_sitter.Query, error) {
	registryOnce.Do(initRegistry)
	spec, ok := langSpecs[langName]
	if !ok || spec.defsQuery == "" {
		return nil, ErrUnsupported
	}
	src, err := queryFS.ReadFile(spec.defsQuery)
	if err != nil {
		return nil, fmt.Errorf("read query %s: %w", spec.defsQuery, err)
	}
	return compileQuery(langName, "defs", string(src))
}

const maxDefinitions = 300

func listDefinitions(path string, content []byte) ([]Definition, error) {
	run, err := runQuery(path, content, queryForLang)
	if err != nil {
		return nil, err
	}
	defer run.Close()

	names := run.query.CaptureNames()

	seen := make(map[string]struct{})
	var defs []Definition
	for cap := range run.Captures() {
		if int(cap.Index) >= len(names) {
			continue
		}
		captureName := names[cap.Index]
		if !strings.HasPrefix(captureName, "name.") {
			continue
		}
		kind := strings.TrimPrefix(captureName, "name.")
		name := strings.TrimSpace(cap.Node.Utf8Text(content))
		if run.langName == "ruby" {
			name = strings.TrimPrefix(name, ":")
		}
		if name == "" {
			continue
		}
		line := int(cap.Node.StartPosition().Row) + 1
		key := fmt.Sprintf("%d:%s:%s", line, kind, name)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		defs = append(defs, Definition{Line: line, Kind: kind, Name: name})
		if len(defs) >= maxDefinitions {
			return defs, nil
		}
	}
	return defs, nil
}
