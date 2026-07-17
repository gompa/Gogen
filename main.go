package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"gogen/internal/agent"
	"gogen/internal/cli"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/debuglog"
	"gogen/internal/llm"
	"gogen/internal/mcp"
	"gogen/internal/projectfile"
	"gogen/internal/server"
	"gogen/internal/session"
	"gogen/internal/treesitter"
	"gogen/internal/tui"
)

func main() {
	cliFlag := flag.Bool("cli", false, "Run interactive TUI mode")
	classicCLIFlag := flag.Bool("classic-cli", false, "Run classic line-oriented CLI (no TUI)")
	webFlag := flag.Bool("web", false, "Run in Web mode")
	hostFlag := flag.String("host", "", "Listen host for --web (e.g. 0.0.0.0, default 127.0.0.1)")
	verboseFlag := flag.Bool("verbose", false, "Show full tool output in CLI mode")
	dirFlag := flag.String("dir", "", "Specify the working directory")
	urlFlag := flag.String("url", "", "OpenAI API base URL (e.g. https://api.openai.com/v1)")
	saveConfigFlag := flag.Bool("save-config", false, "Write effective config to .gogen/gogen.md and exit")
	saveConfigSecretsFlag := flag.Bool("save-config-secrets", false, "Include openai_api_key when using --save-config")
	saveConfigPathFlag := flag.String("save-config-path", "", "Output path for --save-config (default .gogen/gogen.md)")

	flag.Parse()

	workingDir := "."
	if *dirFlag != "" {
		workingDir = *dirFlag
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

	modes := 0
	if *cliFlag {
		modes++
	}
	if *classicCLIFlag {
		modes++
	}
	if *webFlag {
		modes++
	}
	if modes > 1 {
		log.Fatal("Please specify only one of --cli, --classic-cli, or --web")
	}
	if modes == 0 {
		log.Fatal("Please specify --cli, --classic-cli, or --web (or --save-config)")
	}

	if cfg.OpenAIKey == "" {
		log.Fatal("OPENAI_API_KEY environment variable is not set")
	}

	applyRuntimeConfig(cfg)

	provider := llm.NewOpenAIProvider(cfg.OpenAIKey, cfg.OpenAIModel, cfg.OpenAIURL)

	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{
		ContextLimit:         cfg.ContextLimit,
		CompactThreshold:     cfg.CompactThreshold,
		KeepRecentMessages:   cfg.KeepRecentMessages,
		MaxToolResultBytes:   cfg.MaxToolResultBytes,
		CompactReserveTokens: cfg.CompactReserveTokens,
	})
	initCtx := context.Background()
	ctxMgr.EnsureContextLimit(initCtx)
	cfg.OpenAIModel = provider.ModelName()

	exec := agent.NewExecutorWithGuard(cfg.WorkingDir, agent.NewCommandGuard(cfg.CommandSafetyMode, agent.ParseAllowlist(cfg.CommandAllowlist)))
	exec.RequireDeleteApproval = cfg.DeleteApproval != "off"
	a := agent.NewAgent(provider, exec, ctxMgr)
	a.SetProjectContext(cfg.ProjectFilePath, cfg.ProjectGuidelines, cfg.TestCommand, cfg.LintCommand)
	a.TodoManager = agent.NewTodoManager(cfg.WorkingDir)
	a.PinManager = agent.NewPinManager()

	sessionEnabled := os.Getenv("GOGEN_SESSION_PERSIST") != "off"
	store := session.NewStore(sessionEnabled)
	a.SessionStore = store
	a.SessionID = session.NewID()
	if sessionEnabled {
		if latest, err := store.LatestID(cfg.WorkingDir); err == nil && latest != "" {
			if snap, err := store.LoadInWorkingDir(cfg.WorkingDir, latest); err == nil {
				a.RestoreSession(context.Background(), snap)
				a.SessionID = latest
				fmt.Fprintf(os.Stderr, "Session %s (%d msgs)\n", latest, len(a.Messages))
			}
		}
	}
	if name := provider.ModelName(); name != "" {
		fmt.Fprintf(os.Stderr, "Model: %s\n", name)
	} else {
		fmt.Fprintf(os.Stderr, "No model selected; use /models to choose\n")
	}

	var mcpMgr *mcp.Manager
	if cfg.MCPEnabled() && len(cfg.MCPServers) > 0 {
		mcpMgr, err = mcp.NewManager(cfg.MCPServers)
		if err != nil {
			fmt.Fprintf(os.Stderr, "MCP init error: %v\n", err)
		} else if reg := mcpMgr.Registry(); reg != nil {
			a.SetMCPRegistry(reg)
			fmt.Fprintf(os.Stderr, "MCP tools: %d\n", len(reg.ToolNames()))
		}
	}
	if mcpMgr != nil {
		defer mcpMgr.Close()
	}

	// Only catch SIGTERM for program-level shutdown. SIGINT is handled
	// per-turn inside the CLI so a single Ctrl+C does not ruin the session.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	if *webFlag {
		s := server.NewServer(a, cfg)
		addr := cfg.WebBind
		if addr == "" {
			addr = "127.0.0.1:8080"
		} else if !strings.Contains(addr, ":") {
			addr += ":8080"
		}
		fmt.Printf("Starting web server on %s\n", addr)
		if cfg.WebAuthToken != "" {
			fmt.Printf("Auth token required (GOGEN_WEB_TOKEN / web_auth_token)\n")
		}
		if err := s.Start(addr); err != nil {
			log.Fatal(err)
		}
	} else if *classicCLIFlag {
		c := cli.NewCLI(a, cfg)
		c.Run(ctx)
	} else if *cliFlag {
		c := tui.New(a, cfg)
		c.Run(ctx)
	}
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
