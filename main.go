package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
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

type cliFlags struct {
	web               bool
	host              string
	verbose           bool
	dir               string
	url               string
	global            bool
	saveConfig        bool
	saveConfigSecrets bool
	prompt            string
	saveConfigPath    string
}

func parseCLIOptions() (cliFlags, string) {
	opts := cliFlags{}
	flag.BoolVar(&opts.web, "web", false, "Run in Web mode")
	flag.StringVar(&opts.host, "host", "", "Listen host for --web (e.g. 0.0.0.0, default 127.0.0.1)")
	flag.BoolVar(&opts.verbose, "verbose", false, "Show full tool output in CLI mode")
	flag.StringVar(&opts.dir, "dir", "", "Specify the working directory")
	flag.StringVar(&opts.url, "url", "", "OpenAI API base URL (e.g. https://api.openai.com/v1)")
	flag.BoolVar(&opts.global, "global", false, "Run in global mode (ignore project config, use ~/.config/gogen/)")
	flag.BoolVar(&opts.saveConfig, "save-config", false, "Write effective config to .gogen/gogen.md and exit")
	flag.BoolVar(&opts.saveConfigSecrets, "save-config-secrets", false, "Include openai_api_key when using --save-config")
	flag.StringVar(&opts.prompt, "p", "", "Run a single prompt and exit (non-interactive)")
	flag.StringVar(&opts.saveConfigPath, "save-config-path", "", "Output path for --save-config (default .gogen/gogen.md)")

	flag.Parse()

	workingDir := "."
	if opts.dir != "" {
		workingDir = opts.dir
	} else if args := flag.Args(); len(args) > 0 {
		workingDir = args[0]
		if len(args) > 1 {
			if opts.prompt == "" {
				opts.prompt = args[1]
			}
			if len(args) > 2 {
				log.Fatal("Usage: gogen [flags] [dir] [prompt]")
			}
		}
	}
	absWD, err := filepath.Abs(workingDir)
	if err != nil {
		log.Fatal(err)
	}
	return opts, absWD
}

func handleSaveConfigFlag(opts cliFlags, isGlobalMode bool, workingDir string, cfg *config.Config, pf *projectfile.ProjectFile) bool {
	if !opts.saveConfig {
		return false
	}
	if isGlobalMode {
		if err := projectfile.SaveGlobalConfig(cfg, projectfile.WriteOptions{IncludeSecrets: opts.saveConfigSecrets}); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Wrote global config to %s\n", projectfile.GlobalConfigPath())
		fmt.Println("Note: environment variables still override file values at runtime.")
		return true
	}
	outPath := opts.saveConfigPath
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
	if err := projectfile.SaveConfig(outPath, guidelinesPath, cfg, guidelines, projectfile.WriteOptions{IncludeSecrets: opts.saveConfigSecrets}); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Wrote config to %s\n", outPath)
	fmt.Printf("Wrote guidelines to %s\n", guidelinesPath)
	fmt.Println("Note: environment variables still override file values at runtime.")
	return true
}

func main() {
	opts, workingDir := parseCLIOptions()

	profiling.Start()
	defer profiling.Stop()

	isGlobalMode := opts.global || projectfile.IsGlobalModeEnv()

	var verboseOverride *bool
	if opts.verbose {
		v := true
		verboseOverride = &v
	}

	var pf *projectfile.ProjectFile
	if isGlobalMode {
		pf = projectfile.LoadGlobalConfig()
		if pf != nil {
			fmt.Fprintf(os.Stderr, "Using global config from %s\n", pf.Path)
		} else {
			fmt.Fprintf(os.Stderr, "Global mode: no global config found at %s, using defaults\n", projectfile.GlobalConfigPath())
		}
	} else {
		var loadErr error
		pf, loadErr = projectfile.LoadFromWorkingDir(workingDir)
		if loadErr != nil {
			log.Fatalf("project file: %v", loadErr)
		}
	}

	cfg := projectfile.Merge(pf, projectfile.FlagOverrides{
		WorkingDir: workingDir,
		OpenAIURL:  opts.url,
		CLIVerbose: verboseOverride,
		WebBind:    opts.host,
	})
	if pf != nil {
		cfg.ProjectGuidelines = pf.Guidelines
		cfg.ProjectFilePath = pf.Path
	}

	if handleSaveConfigFlag(opts, isGlobalMode, workingDir, cfg, pf) {
		return
	}

	if cfg.OpenAIKey == "" {
		fmt.Fprintf(os.Stderr, "Warning: OPENAI_API_KEY is not set. Some endpoints may require an API key.\n")
	}

	applyRuntimeConfig(cfg)

	// Pre-load the cl100k_base tokenizer so the first token-counting call
	// does not block on the ~2.6 MB init overhead.
	contextmgr.WarmTokenizer()

	provider := llm.NewOpenAIProvider(cfg.OpenAIKey, cfg.OpenAIModel, cfg.OpenAIURL, cfg.WorkingDir)

	// Derive a stable prompt-cache key from the working directory so
	// provider-side prefix caches survive sequential requests.
	provider.SetPromptCacheKey(llm.ProjectPromptCacheKey(cfg.WorkingDir))
	provider.SetPreserveReasoningMode(cfg.PreserveReasoning)

	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{
		ContextLimit:         cfg.ContextLimit,
		CompactThreshold:     cfg.CompactThreshold,
		KeepRecentMessages:   cfg.KeepRecentMessages,
		MaxToolResultBytes:   cfg.MaxToolResultBytes,
		CompactReserveTokens: cfg.CompactReserveTokens,
	})

	exec := agent.NewExecutorWithGuard(cfg.WorkingDir, agent.NewCommandGuard(cfg.CommandSafetyMode, agent.ParseAllowlist(cfg.CommandAllowlist)))
	exec.RequireDeleteApproval = !strings.EqualFold(cfg.DeleteApproval, "off")
	exec.Sandbox = cfg.CommandSandbox
	if cfg.CommandTimeoutSecs > 0 {
		exec.CommandTimeout = time.Duration(cfg.CommandTimeoutSecs) * time.Second
	}
	if isGlobalMode {
		// In global mode, relax the path boundary to the user's home directory.
		exec.PathBoundary = projectfile.GlobalPathBoundary()
	}
	a := agent.NewAgent(provider, exec, ctxMgr)
	a.GlobalMode = isGlobalMode
	a.SetProjectContext(cfg.ProjectFilePath, cfg.ProjectGuidelines, cfg.TestCommand, cfg.LintCommand)
	a.TodoManager = agent.NewTodoManager(cfg.WorkingDir)
	a.PinManager = agent.NewPinManager()
	a.DebugCompareMessages = cfg.DebugCompareMessages
	if cfg.DebugCompareMessages && !agent.ViewDriftCompiledIn() {
		fmt.Fprintf(os.Stderr, "GOGEN_DEBUG_COMPARE_MESSAGES requires a debug build (-tags debug); ignoring\n")
		a.DebugCompareMessages = false
	}

	sessionEnabled := !strings.EqualFold(os.Getenv("GOGEN_SESSION_PERSIST"), "off")
	sessionOpts := session.StoreOptions{
		MaxCount:   cfg.SessionMaxCount,
		MaxAgeDays: cfg.SessionMaxAgeDays,
	}
	store := session.NewStoreWithOptions(sessionEnabled, sessionOpts)
	if isGlobalMode {
		// Use global session dir ~/.local/share/gogen/sessions/
		store.SetGlobalDir(projectfile.GlobalSessionDir())
	}
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
	mcpDone := make(chan struct{})
	initMCP := func() {
		defer close(mcpDone)
		servers := mcp.ValidServers(cfg.MCPServers)
		if !cfg.MCPEnabled() {
			if len(cfg.MCPServers) > 0 {
				fmt.Fprintf(os.Stderr, "MCP servers configured but mcp is off; set mcp: on or GOGEN_MCP=on to enable\n")
			}
			return
		}
		if len(servers) == 0 {
			// mcp: on with no usable servers — do not start a manager.
			if len(cfg.MCPServers) > 0 {
				fmt.Fprintf(os.Stderr, "MCP enabled but no valid mcp_servers entries (need name + command)\n")
			}
			return
		}
		var mcpErr error
		mcpMgr, mcpErr = mcp.NewManager(servers)
		if mcpErr != nil {
			fmt.Fprintf(os.Stderr, "MCP init error: %v\n", mcpErr)
		} else if reg := mcpMgr.Registry(); reg != nil {
			a.SetMCPRegistry(reg)
			fmt.Fprintf(os.Stderr, "MCP tools: %d\n", len(reg.ToolNames()))
		}
	}
	defer func() {
		select {
		case <-mcpDone:
		case <-time.After(3 * time.Second):
			log.Printf("mcp shutdown: timed out waiting for init")
		}
		if mcpMgr == nil {
			return
		}
		done := make(chan struct{})
		go func() {
			_ = mcpMgr.Close()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			log.Printf("mcp shutdown: timed out closing manager")
		}
	}()

	// Inherited SIG_IGN sticks across Notify unless cleared first.
	signal.Reset(syscall.SIGINT, syscall.SIGTERM)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	defer a.FlushSession()

	if opts.prompt != "" {
		go initMCP()
		go a.ValidateRestoredModel(context.Background(), restoredModel)
		runSinglePrompt(ctx, a, opts.prompt, cfg)
		return
	}

	if opts.web {
		// Determine the listen address first so we can check for loopback
		// and auto-generate a token before creating the server.
		addr := cfg.WebBind
		var isLoopback bool
		if addr == "" {
			addr = "127.0.0.1:8081"
			isLoopback = true
		} else {
			if !strings.Contains(addr, ":") {
				addr += ":8081"
			}
			isLoopback = server.IsLoopbackBind(addr)
		}

		// For non-loopback binds, auto-generate a token if none is provided.
		if !isLoopback && cfg.WebAuthToken == "" {
			token, err := generateToken()
			if err != nil {
				log.Fatalf("failed to generate auth token: %v", err)
			}
			cfg.WebAuthToken = token
		}

		s := server.NewServer(a, cfg)

		// Build a user-friendly URL for the startup message.
		// Replace 0.0.0.0 with 127.0.0.1 so the link works when clicked.
		displayAddr := addr
		if strings.HasPrefix(displayAddr, "0.0.0.0:") {
			displayAddr = "127.0.0.1:" + displayAddr[len("0.0.0.0:"):]
		}
		if cfg.WebAuthToken != "" {
			fmt.Printf("Open http://%s?token=%s\n", displayAddr, cfg.WebAuthToken)
		} else {
			fmt.Printf("Open http://%s\n", displayAddr)
		}
		// Listen first so the UI can connect immediately. Provider model
		// validation and context-limit lookup continue in the background.
		// MCP only starts when there is at least one valid server configured.
		errCh := make(chan error, 1)
		go func() {
			errCh <- s.Start(ctx, addr)
		}()
		go func() {
			a.ValidateRestoredModel(context.Background(), restoredModel)
			cfg.OpenAIModel = provider.ModelName()
		}()
		go initMCP()
		if err := <-errCh; err != nil {
			log.Printf("web server error: %v", err)
		}
		return
	}

	// MCP (if configured) + model validation in the background so the TUI
	// can open immediately.
	go initMCP()
	go a.ValidateRestoredModel(context.Background(), restoredModel)
	// Default: TUI mode.
	c := tui.New(a, cfg)
	c.Run(ctx)
}

// runSinglePrompt processes one user prompt and exits.
// It uses the already-initialized agent and config but skips the TUI/web.
func runSinglePrompt(ctx context.Context, a *agent.Agent, prompt string, cfg *config.Config) {
	// In single-prompt mode there is no interactive approval modal, so a
	// delete that requires approval would be blocked (safely) by the
	// ErrDeleteApprovalRequired error. Only skip the approval check when the
	// user explicitly opted out via delete_approval: off; otherwise warn so
	// the block is not surprising.
	if strings.EqualFold(cfg.DeleteApproval, "off") {
		a.Executor.RequireDeleteApproval = false
	} else if a.Executor.RequireDeleteApproval {
		fmt.Fprintf(os.Stderr, "Note: delete_file requires approval (delete_approval: %s) and is blocked in single-prompt mode; set GOGEN_DELETE_APPROVAL=off to allow deletes.\n", cfg.DeleteApproval)
	}

	// Start a fresh session: clear any restored conversation state.
	a.ResetSessionState()
	a.SessionID = session.NewID()
	a.SessionOneshot = true
	a.FlushSession()

	var final strings.Builder
	handlers := &llm.StreamHandlers{
		OnStart: func() {
			if cfg.CLIVerbose {
				fmt.Fprintln(os.Stderr, "─── prompt ───")
			}
		},
		OnToken: func(token string) {
			final.WriteString(token)
			if cfg.CLIVerbose {
				fmt.Print(token)
			}
		},
		OnStreamEnd: func() {
			if cfg.CLIVerbose {
				fmt.Fprintln(os.Stderr, "")
			}
		},
	}

	_, err := a.StreamProcessInput(ctx, prompt, handlers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nError: %v\n", err)
		os.Exit(1)
	}
	if !cfg.CLIVerbose {
		fmt.Println(final.String())
	}
}

// generateToken returns a cryptographically random 32-byte hex string.
func generateToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
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
