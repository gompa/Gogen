package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/debuglog"
	"gogen/internal/llm"
	"gogen/internal/mcp"
	"gogen/internal/profiling"
	"gogen/internal/projectfile"
	"gogen/internal/server"
	"gogen/internal/session"
	"gogen/internal/treesitter"
	"gogen/internal/tui"
)

func main() {
	webFlag := flag.Bool("web", false, "Run in Web mode")
	hostFlag := flag.String("host", "", "Listen host for --web (e.g. 0.0.0.0, default 127.0.0.1)")
	verboseFlag := flag.Bool("verbose", false, "Show full tool output in CLI mode")
	dirFlag := flag.String("dir", "", "Specify the working directory")
	urlFlag := flag.String("url", "", "OpenAI API base URL (e.g. https://api.openai.com/v1)")
	saveConfigFlag := flag.Bool("save-config", false, "Write effective config to .gogen/gogen.md and exit")
	saveConfigSecretsFlag := flag.Bool("save-config-secrets", false, "Include openai_api_key when using --save-config")
	saveConfigPathFlag := flag.String("save-config-path", "", "Output path for --save-config (default .gogen/gogen.md)")

	flag.Parse()

	profiling.Start()
	defer profiling.Stop()

	workingDir := "."
	if *dirFlag != "" {
		workingDir = *dirFlag
	} else if args := flag.Args(); len(args) > 0 {
		workingDir = args[0]
		if len(args) > 1 {
			log.Fatal("Only one positional argument (working directory) is accepted")
		}
	}
	absWD, err := filepath.Abs(workingDir)
	if err != nil {
		log.Fatal(err)
	}
	workingDir = absWD

	var verboseOverride *bool
	if *verboseFlag {
		v := true
		verboseOverride = &v
	}

	pf, err := projectfile.LoadFromWorkingDir(workingDir)
	if err != nil {
		log.Fatalf("project file: %v", err)
	}

	cfg := projectfile.Merge(pf, projectfile.FlagOverrides{
		WorkingDir: workingDir,
		OpenAIURL:  *urlFlag,
		CLIVerbose: verboseOverride,
		WebBind:    *hostFlag,
	})
	if pf != nil {
		cfg.ProjectGuidelines = pf.Guidelines
		cfg.ProjectFilePath = pf.Path
	}

	if *saveConfigFlag {
		outPath := *saveConfigPathFlag
		if outPath == "" {
			outPath = projectfile.DefaultSavePath(workingDir)
		} else if !filepath.IsAbs(outPath) {
			outPath = filepath.Join(workingDir, outPath)
		}
		guidelinesPath := projectfile.DefaultGuidelinesSavePath(workingDir)
		guidelines := cfg.ProjectGuidelines
		if pf != nil && guidelines == "" {
			guidelines = pf.Guidelines
		}
		if err := projectfile.SaveConfig(outPath, guidelinesPath, cfg, guidelines, projectfile.WriteOptions{IncludeSecrets: *saveConfigSecretsFlag}); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Wrote config to %s\n", outPath)
		fmt.Printf("Wrote guidelines to %s\n", guidelinesPath)
		fmt.Println("Note: environment variables still override file values at runtime.")
		return
	}

	if cfg.OpenAIKey == "" {
		fmt.Fprintf(os.Stderr, "Warning: OPENAI_API_KEY is not set. Some endpoints may require an API key.\n")
	}

	applyRuntimeConfig(cfg)

	provider := llm.NewOpenAIProvider(cfg.OpenAIKey, cfg.OpenAIModel, cfg.OpenAIURL)

	// Derive a stable prompt-cache key from the working directory so
	// provider-side prefix caches survive sequential requests.
	promptKey := projectPromptCacheKey(cfg.WorkingDir)
	provider.SetPromptCacheKey(promptKey)

	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{
		ContextLimit:         cfg.ContextLimit,
		CompactThreshold:     cfg.CompactThreshold,
		KeepRecentMessages:   cfg.KeepRecentMessages,
		MaxToolResultBytes:   cfg.MaxToolResultBytes,
		CompactReserveTokens: cfg.CompactReserveTokens,
	})

	exec := agent.NewExecutorWithGuard(cfg.WorkingDir, agent.NewCommandGuard(cfg.CommandSafetyMode, agent.ParseAllowlist(cfg.CommandAllowlist)))
	exec.RequireDeleteApproval = cfg.DeleteApproval != "off"
	exec.Sandbox = cfg.CommandSandbox
	if cfg.CommandTimeoutSecs > 0 {
		exec.CommandTimeout = time.Duration(cfg.CommandTimeoutSecs) * time.Second
	}
	a := agent.NewAgent(provider, exec, ctxMgr)
	a.SetProjectContext(cfg.ProjectFilePath, cfg.ProjectGuidelines, cfg.TestCommand, cfg.LintCommand)
	a.TodoManager = agent.NewTodoManager(cfg.WorkingDir)
	a.PinManager = agent.NewPinManager()
	a.DebugCompareMessages = cfg.DebugCompareMessages
	if cfg.DebugCompareMessages && !agent.ViewDriftCompiledIn() {
		fmt.Fprintf(os.Stderr, "GOGEN_DEBUG_COMPARE_MESSAGES requires a debug build (-tags debug); ignoring\n")
		a.DebugCompareMessages = false
	}

	sessionEnabled := os.Getenv("GOGEN_SESSION_PERSIST") != "off"
	store := session.NewStoreWithOptions(sessionEnabled, session.StoreOptions{
		MaxCount:   cfg.SessionMaxCount,
		MaxAgeDays: cfg.SessionMaxAgeDays,
	})
	a.SessionStore = store
	a.SessionID = session.NewID()
	// Local-only restore: avoid blocking startup on provider ListModels.
	var restoredModel string
	if sessionEnabled {
		if latest, err := store.LatestID(cfg.WorkingDir); err == nil && latest != "" {
			if snap, err := store.LoadInWorkingDir(cfg.WorkingDir, latest); err == nil {
				a.RestoreSessionLocal(snap, latest)
				a.SessionID = latest
				restoredModel = snap.Model
				fmt.Fprintf(os.Stderr, "Session %s (%d msgs)\n", latest, len(a.Messages))
			}
		}
	}
	// One-time migration: adopt project-global todos.json into the current
	// session when it has no todos yet, then rename the legacy file.
	if a.ImportLegacyTodos() {
		fmt.Fprintf(os.Stderr, "Migrated legacy todos into session %s\n", a.SessionID)
	}
	if name := provider.ModelName(); name != "" {
		fmt.Fprintf(os.Stderr, "Model: %s\n", name)
	} else {
		fmt.Fprintf(os.Stderr, "No model selected; use /models to choose\n")
	}

	var mcpMgr *mcp.Manager
	initMCP := func() {
		if cfg.MCPEnabled() && len(cfg.MCPServers) > 0 {
			var mcpErr error
			mcpMgr, mcpErr = mcp.NewManager(cfg.MCPServers)
			if mcpErr != nil {
				fmt.Fprintf(os.Stderr, "MCP init error: %v\n", mcpErr)
			} else if reg := mcpMgr.Registry(); reg != nil {
				a.SetMCPRegistry(reg)
				fmt.Fprintf(os.Stderr, "MCP tools: %d\n", len(reg.ToolNames()))
			}
		} else if !cfg.MCPEnabled() && len(cfg.MCPServers) > 0 {
			fmt.Fprintf(os.Stderr, "MCP servers configured but mcp is off; set mcp: on or GOGEN_MCP=on to enable\n")
		}
	}
	defer func() {
		if mcpMgr != nil {
			_ = mcpMgr.Close()
		}
	}()

	// Catch SIGTERM and SIGINT for program-level shutdown. In TUI mode
	// SIGINT is also handled per-turn (cancel vs quit); cancelling this
	// context on a second Ctrl+C / quit path is harmless. In web mode,
	// Start watches ctx so Ctrl+C shuts the HTTP server down cleanly and
	// the deferred FlushSession runs.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	defer a.FlushSession()

	if *webFlag {
		s := server.NewServer(a, cfg)
		addr := cfg.WebBind
		if addr == "" {
			addr = "127.0.0.1:8081"
		} else if !strings.Contains(addr, ":") {
			addr += ":8081"
		}
		fmt.Printf("Starting web server on %s\n", addr)
		if cfg.WebAuthToken != "" {
			fmt.Printf("Auth token required (GOGEN_WEB_TOKEN / web_auth_token)\n")
		}
		// Listen first so the UI can connect immediately. Provider model
		// validation and context-limit lookup continue in the background —
		// start them before MCP init so a slow MCP server cannot delay the
		// catalog warm-up that list_models joins.
		errCh := make(chan error, 1)
		go func() {
			errCh <- s.Start(ctx, addr)
		}()
		go func() {
			a.ValidateRestoredModel(context.Background(), restoredModel)
			cfg.OpenAIModel = provider.ModelName()
		}()
		initMCP()
		if err := <-errCh; err != nil {
			log.Printf("web server error: %v", err)
		}
		return
	}

	initMCP()
	// Provider model validation and context-limit lookup continue in the
	// background so the TUI can open immediately (same as web mode).
	go a.ValidateRestoredModel(context.Background(), restoredModel)
	// Default: TUI mode.
	c := tui.New(a, cfg)
	c.Run(ctx)
}

func applyRuntimeConfig(cfg *config.Config) {
	treesitter.Configure(cfg.TreeSitterEnabled(), cfg.TreeSitterLangs)
	agent.ConfigureWebFetch(cfg.WebFetchEnabled(), cfg.WebFetchMode, cfg.WebAllowedDomains)
	agent.ConfigureWebSearchEnabled(cfg.WebSearchEnabled())
	agent.ConfigureWebSearch(cfg.WebSearchBackend, cfg.WebSearchAPIKey)
	if cfg.DebugLog != "" || cfg.DebugSession != "" {
		debuglog.Configure(cfg.DebugLog, cfg.DebugSession)
	}
}

// projectPromptCacheKey returns a stable, short hash of the working directory
// for use as an OpenAI prompt_cache_key. This keeps provider-side cache hits
// scoped per-project without leaking paths.
func projectPromptCacheKey(workingDir string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(workingDir))
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], h.Sum64())
	return hex.EncodeToString(b[:])
}
