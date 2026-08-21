module gogen

go 1.27.0

require (
	codeberg.org/readeck/go-readability/v2 v2.1.2
	github.com/JohannesKaufmann/html-to-markdown/v2 v2.5.2
	github.com/atotto/clipboard v0.1.4
	github.com/aymanbagabas/go-osc52/v2 v2.0.1
	github.com/charmbracelet/bubbles v1.0.0
	github.com/charmbracelet/bubbletea v1.3.10
	github.com/charmbracelet/lipgloss v1.1.0
	github.com/charmbracelet/x/ansi v0.11.8
	github.com/creack/pty v1.1.24
	github.com/ericchiang/css v1.4.0
	github.com/gorilla/websocket v1.5.3
	github.com/gortexhq/tree-sitter-dockerfile v0.1.0
	github.com/makiuchi-d/gozxing v0.1.1
	github.com/muesli/reflow v0.3.0
	github.com/openai/openai-go v1.12.0
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
	github.com/tiktoken-go/tokenizer v0.8.1
	github.com/tree-sitter-grammars/tree-sitter-hcl v1.2.0
	github.com/tree-sitter-grammars/tree-sitter-kotlin v1.1.0
	github.com/tree-sitter-grammars/tree-sitter-lua v0.5.0
	github.com/tree-sitter-grammars/tree-sitter-make v1.1.1
	github.com/tree-sitter-grammars/tree-sitter-toml v0.7.0
	github.com/tree-sitter-grammars/tree-sitter-yaml v0.7.2
	github.com/tree-sitter-grammars/tree-sitter-zig v1.1.2
	github.com/tree-sitter/go-tree-sitter v0.25.0
	github.com/tree-sitter/tree-sitter-bash v0.25.1
	github.com/tree-sitter/tree-sitter-c v0.24.2
	github.com/tree-sitter/tree-sitter-c-sharp v0.23.5
	github.com/tree-sitter/tree-sitter-cpp v0.23.4
	github.com/tree-sitter/tree-sitter-css v0.25.0
	github.com/tree-sitter/tree-sitter-go v0.25.0
	github.com/tree-sitter/tree-sitter-html v0.23.2
	github.com/tree-sitter/tree-sitter-java v0.23.5
	github.com/tree-sitter/tree-sitter-javascript v0.25.0
	github.com/tree-sitter/tree-sitter-json v0.24.8
	github.com/tree-sitter/tree-sitter-php v0.24.2
	github.com/tree-sitter/tree-sitter-python v0.25.0
	github.com/tree-sitter/tree-sitter-ruby v0.23.1
	github.com/tree-sitter/tree-sitter-rust v0.24.2
	github.com/tree-sitter/tree-sitter-scala v0.26.2
	github.com/tree-sitter/tree-sitter-typescript v0.23.2
	github.com/wippyai/tree-sitter-sql v0.0.4
	golang.org/x/net v0.58.0
	golang.org/x/time v0.15.0
	gopkg.in/yaml.v3 v3.0.1
	mvdan.cc/sh/v3 v3.13.1
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/JohannesKaufmann/dom v0.3.1 // indirect
	github.com/andybalholm/cascadia v1.3.4 // indirect
	github.com/charmbracelet/colorprofile v0.4.3 // indirect
	github.com/charmbracelet/x/cellbuf v0.0.15 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/dlclark/regexp2/v2 v2.7.1 // indirect
	github.com/erikgeiser/coninput v0.0.0-20211004153227-1c3628e74d0f // indirect
	github.com/fzipp/gocyclo v0.6.0 // indirect
	github.com/go-shiori/dom v0.0.0-20230515143342-73569d674e1c // indirect
	github.com/gogs/chardet v0.0.0-20211120154057-b7413eaefb8f // indirect
	github.com/itlightning/dateparse v0.2.1 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mattn/go-localereader v0.0.1 // indirect
	github.com/mattn/go-pointer v0.0.1 // indirect
	github.com/mattn/go-runewidth v0.0.27 // indirect
	github.com/muesli/ansi v0.0.0-20230316100256-276c6243b2f6 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/muesli/termenv v0.16.0 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/tidwall/gjson v1.19.0 // indirect
	github.com/tidwall/match v1.2.0 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/xo/terminfo v1.0.0 // indirect
	golang.org/x/exp/typeparams v0.0.0-20260813180055-c1d0aacb2297 // indirect
	golang.org/x/mod v0.40.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/telemetry v0.0.0-20260814151720-d8c169486af1 // indirect
	golang.org/x/term v0.45.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	golang.org/x/tools v0.49.0 // indirect
	golang.org/x/vuln v1.7.0 // indirect
	golang.org/x/xerrors v0.0.0-20200804184101-5ec99f83aff1 // indirect
	honnef.co/go/tools v0.7.0 // indirect
)

tool (
	github.com/fzipp/gocyclo/cmd/gocyclo
	golang.org/x/vuln/cmd/govulncheck
	honnef.co/go/tools/cmd/staticcheck
)
