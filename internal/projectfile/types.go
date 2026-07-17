package projectfile

// MCPServerEntry describes one MCP stdio server.
type MCPServerEntry struct {
	Name    string            `yaml:"name"`
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
}

// FileConfig holds parsed YAML front matter keys and which keys were present.
type FileConfig struct {
	Keys map[string]struct{}

	OpenAIAPIKey         string
	OpenAIModel          string
	OpenAIBaseURL        string
	WorkingDir           string
	ContextLimit         int
	CompactThreshold     float64
	KeepRecentMessages   int
	MaxToolResultBytes   int
	CompactReserveTokens int
	CommandSafety        string
	CommandAllowlist     string
	DeleteApproval       string
	TreeSitter           string
	TreeSitterLangs      string
	CLIVerbose           bool
	DebugLog             string
	DebugSession         string
	MCP                  string
	MCPServers           []MCPServerEntry
	TestCommand          string
	LintCommand          string
	WebFetch             string
	WebSearch            string
	WebSearchBackend     string
	WebSearchAPIKey      string
	WebAllowedDomains    string
	WebFetchMode         string
	WebAuthToken         string
}

// ProjectFile is a loaded combined config + guidelines file.
type ProjectFile struct {
	Path       string
	HasConfig  bool
	Guidelines string
	Config     FileConfig
}

// FlagOverrides are CLI flag values applied after env/file merge.
type FlagOverrides struct {
	WorkingDir string
	OpenAIURL  string
	CLIVerbose *bool
	WebBind    string
}

// WriteOptions controls SaveConfig output.
type WriteOptions struct {
	IncludeSecrets bool
}

const defaultGuidelinesPlaceholder = "# Project guidelines\n\nAdd agent instructions for this repository here.\n"
