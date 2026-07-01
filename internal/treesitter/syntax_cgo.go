//go:build cgo

package treesitter

import (
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_bash "github.com/tree-sitter/tree-sitter-bash/bindings/go"
	tree_sitter_c "github.com/tree-sitter/tree-sitter-c/bindings/go"
	tree_sitter_c_sharp "github.com/tree-sitter/tree-sitter-c-sharp/bindings/go"
	tree_sitter_cpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	tree_sitter_css "github.com/tree-sitter/tree-sitter-css/bindings/go"
	tree_sitter_go "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tree_sitter_html "github.com/tree-sitter/tree-sitter-html/bindings/go"
	tree_sitter_java "github.com/tree-sitter/tree-sitter-java/bindings/go"
	tree_sitter_javascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tree_sitter_json "github.com/tree-sitter/tree-sitter-json/bindings/go"
	tree_sitter_php "github.com/tree-sitter/tree-sitter-php/bindings/go"
	tree_sitter_python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tree_sitter_ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
	tree_sitter_rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
	tree_sitter_hcl "github.com/tree-sitter-grammars/tree-sitter-hcl/bindings/go"
	tree_sitter_lua "github.com/tree-sitter-grammars/tree-sitter-lua/bindings/go"
	tree_sitter_toml "github.com/tree-sitter-grammars/tree-sitter-toml/bindings/go"
	tree_sitter_yaml "github.com/tree-sitter-grammars/tree-sitter-yaml/bindings/go"
)

type langSpec struct {
	name  string
	exts  []string
	ptrFn func() unsafe.Pointer
}

var (
	registryOnce sync.Once
	extToLang    map[string]string
	langSpecs    map[string]langSpec
	langCache    sync.Map // string -> *tree_sitter.Language
)

func bundledSpecs() []langSpec {
	return []langSpec{
		{name: "go", exts: []string{"go"}, ptrFn: tree_sitter_go.Language},
		{name: "python", exts: []string{"py", "pyi"}, ptrFn: tree_sitter_python.Language},
		{name: "javascript", exts: []string{"js", "mjs", "cjs"}, ptrFn: tree_sitter_javascript.Language},
		{name: "typescript", exts: []string{"ts", "mts", "cts"}, ptrFn: tree_sitter_typescript.LanguageTypescript},
		{name: "tsx", exts: []string{"tsx"}, ptrFn: tree_sitter_typescript.LanguageTSX},
		{name: "json", exts: []string{"json"}, ptrFn: tree_sitter_json.Language},
		{name: "rust", exts: []string{"rs"}, ptrFn: tree_sitter_rust.Language},
		{name: "java", exts: []string{"java"}, ptrFn: tree_sitter_java.Language},
		{name: "c", exts: []string{"c", "h"}, ptrFn: tree_sitter_c.Language},
		{name: "cpp", exts: []string{"cpp", "cc", "cxx", "hpp", "hh", "hxx"}, ptrFn: tree_sitter_cpp.Language},
		{name: "csharp", exts: []string{"cs"}, ptrFn: tree_sitter_c_sharp.Language},
		{name: "php", exts: []string{"php"}, ptrFn: tree_sitter_php.LanguagePHP},
		{name: "ruby", exts: []string{"rb"}, ptrFn: tree_sitter_ruby.Language},
		{name: "html", exts: []string{"html", "htm"}, ptrFn: tree_sitter_html.Language},
		{name: "css", exts: []string{"css"}, ptrFn: tree_sitter_css.Language},
		{name: "bash", exts: []string{"sh", "bash"}, ptrFn: tree_sitter_bash.Language},
		{name: "yaml", exts: []string{"yaml", "yml"}, ptrFn: tree_sitter_yaml.Language},
		{name: "toml", exts: []string{"toml"}, ptrFn: tree_sitter_toml.Language},
		{name: "lua", exts: []string{"lua"}, ptrFn: tree_sitter_lua.Language},
		{name: "hcl", exts: []string{"hcl", "tf"}, ptrFn: tree_sitter_hcl.Language},
	}
}

func initRegistry() {
	extToLang = make(map[string]string)
	langSpecs = make(map[string]langSpec)
	for _, spec := range bundledSpecs() {
		langSpecs[spec.name] = spec
		for _, ext := range spec.exts {
			extToLang[ext] = spec.name
		}
	}
}

func BundledLanguages() []string {
	registryOnce.Do(initRegistry)
	allowed := allowedLangs()
	names := make([]string, 0, len(langSpecs))
	for name := range langSpecs {
		if langAllowed(name, allowed) {
			names = append(names, name)
		}
	}
	return names
}

func languageFor(name string) *tree_sitter.Language {
	if v, ok := langCache.Load(name); ok {
		return v.(*tree_sitter.Language)
	}
	spec, ok := langSpecs[name]
	if !ok || spec.ptrFn == nil {
		return nil
	}
	lang := tree_sitter.NewLanguage(spec.ptrFn())
	langCache.Store(name, lang)
	return lang
}

func langNameForPath(path string) (string, bool) {
	registryOnce.Do(initRegistry)
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	if ext == "" {
		return "", false
	}
	name, ok := extToLang[ext]
	if !ok || !langAllowed(name, allowedLangs()) {
		return "", false
	}
	return name, true
}

func checkSupported(path string, content []byte) []Issue {
	registryOnce.Do(initRegistry)

	langName, ok := langNameForPath(path)
	if !ok {
		return nil
	}
	lang := languageFor(langName)
	if lang == nil {
		return nil
	}
	return parseIssues(lang, content)
}

func parseIssues(lang *tree_sitter.Language, content []byte) []Issue {
	if len(content) == 0 {
		return nil
	}

	parser := tree_sitter.NewParser()
	defer parser.Close()
	parser.SetLanguage(lang)

	tree := parser.Parse(content, nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()

	root := tree.RootNode()
	if !root.HasError() {
		return nil
	}

	seen := make(map[int]struct{})
	var issues []Issue
	collectIssues(root, &issues, seen)
	return issues
}

func collectIssues(n *tree_sitter.Node, issues *[]Issue, seen map[int]struct{}) {
	if n == nil {
		return
	}
	if n.IsError() || n.IsMissing() {
		line := int(n.StartPosition().Row) + 1
		if _, dup := seen[line]; !dup {
			seen[line] = struct{}{}
			msg := "syntax error"
			if n.IsMissing() {
				msg = "missing token"
			}
			*issues = append(*issues, Issue{Line: line, Message: msg})
		}
	}
	for i := uint(0); i < n.ChildCount(); i++ {
		collectIssues(n.Child(i), issues, seen)
	}
}
