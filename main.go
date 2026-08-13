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

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/debuglog"
	"gogen/internal/llm"
	"gogen/internal/profiling"
	"gogen/internal/projectfile"
	"gogen/internal/session"
	"gogen/internal/treesitter"
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
	flag.BoolVar(&opts.saveConfig, "save-config", false, "Write effective config to .gogen/gogen.conf and guidelines to .gogen/gogen.md, then exit")
	flag.BoolVar(&opts.saveConfigSecrets, "save-config-secrets", false, "Include openai_api_key when using --save-config")
	flag.StringVar(&opts.prompt, "p", "", "Run a single prompt and exit (non-interactive)")
	flag.StringVar(&opts.saveConfigPath, "save-config-path", "", "Output path for --save-config config file (default .gogen/gogen.conf)")

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
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nError: %v\n", err)
		os.Exit(1)
	}
}

// run executes the selected mode (single prompt, web, or TUI) and returns an
// error for the single-prompt path. All deferred cleanup (agent close,
// session flush, MCP shutdown, profiling stop) is registered here so it runs
// on both the success and error paths; main only translates a returned error
// into a non-zero exit code.
func run() error {
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
		return nil
	}

	// Workspace instruction files (AGENTS.md / CLAUDE.md) are merged below
	// the project guidelines at VIEW-BUILD time from the agent's current
	// working dir (agent.EffectiveGuidelines), so a /dir or web workspace
	// change re-renders them and the content is never baked into a saved
	// .gogen/gogen.md.

	if cfg.OpenAIKey == "" {
		fmt.Fprintf(os.Stderr, "Warning: OPENAI_API_KEY is not set. Some endpoints may require an API key.\n")
	}

	applyRuntimeConfig(cfg)

	// Pre-load the cl100k_base tokenizer so the first token-counting call
	// does not block on the ~2.6 MB init overhead.
	contextmgr.WarmTokenizer()

	a, restoredModel := newAgent(cfg, isGlobalMode)
	// Background jobs (execute_command background=true) are owned by the
	// session and killed when it closes; this defer covers the TUI, CLI, and
	// web default-session agents at process exit (web session agents are
	// closed by ShutdownSessions). Idempotent.
	defer a.Close()

	mcpH := startMCP(a, cfg)
	defer closeMCP(mcpH)

	// Inherited SIG_IGN sticks across Notify unless cleared first.
	signal.Reset(syscall.SIGINT, syscall.SIGTERM)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	defer a.FlushPending()

	if opts.prompt != "" {
		go a.ValidateRestoredModel(context.Background(), restoredModel)
		return runSinglePrompt(ctx, a, opts.prompt, cfg)
	}

	if opts.web {
		runWeb(ctx, a, cfg, restoredModel)
		return nil
	}

	// Default: TUI mode.
	runTUI(ctx, a, cfg, restoredModel)
	return nil
}

// runSinglePrompt processes one user prompt. It returns the error so the
// caller can exit non-zero after the deferred cleanup in run has executed.
// It uses the already-initialized agent and config but skips the TUI/web.
func runSinglePrompt(ctx context.Context, a *agent.Agent, prompt string, cfg *config.Config) error {
	// In single-prompt mode there is no interactive approval modal, so a
	// delete that requires approval would be blocked (safely) by the
	// ErrDeleteApprovalRequired error. Only skip the approval check when the
	// user explicitly opted out via delete_approval: off; otherwise warn so
	// the block is not surprising.
	if strings.EqualFold(cfg.DeleteApproval, "off") {
		a.Executor.SetDeleteApproval(false)
	} else if a.Executor.DeleteApprovalRequired() {
		fmt.Fprintf(os.Stderr, "Note: delete requires approval (delete_approval: %s) and is blocked in single-prompt mode; set GOGEN_DELETE_APPROVAL=off to allow deletes.\n", cfg.DeleteApproval)
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
		return err
	}
	if !cfg.CLIVerbose {
		fmt.Println(final.String())
	}
	return nil
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
	agent.ConfigureSystemPrompt(cfg.SystemPrompt)
	agent.ConfigureSubagentPrompt(cfg.SubagentPrompt)
	if cfg.DebugLog != "" || cfg.DebugSession != "" {
		debuglog.Configure(cfg.DebugLog, cfg.DebugSession)
	}
}
