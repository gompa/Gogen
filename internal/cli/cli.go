package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm"
	"gogen/internal/projectfile"
	"gogen/internal/session"
)

type CLI struct {
	agent   *agent.Agent
	cfg     *config.Config
	verbose bool
}

func NewCLI(a *agent.Agent, cfg *config.Config) *CLI {
	verbose := cfg != nil && cfg.CLIVerbose
	return &CLI{agent: a, cfg: cfg, verbose: verbose}
}

// SetVerbose controls whether full tool results are printed.
func (c *CLI) SetVerbose(v bool) {
	c.verbose = v
}

func (c *CLI) Run(ctx context.Context) {
	// Single SIGINT owner for the whole session: cancel the active turn only.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGINT)
	defer signal.Stop(sigCh)

	var turnCancel context.CancelFunc
	var turnMu sync.Mutex
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sigCh:
				turnMu.Lock()
				if turnCancel != nil {
					turnCancel()
				}
				turnMu.Unlock()
			}
		}
	}()
	defer func() {
		turnMu.Lock()
		if turnCancel != nil {
			turnCancel()
		}
		turnMu.Unlock()
	}()

	fmt.Printf("GoGen — %s (%s)\n", c.agent.Executor.WorkingDir, c.agent.Mode.String())
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  exit              quit")
	fmt.Println("  dir <path>        change working directory")
	fmt.Println("  compact           summarize conversation history")
	fmt.Println("  /models           list or switch models")
	fmt.Println("  /context          show context usage")
	fmt.Println("  /plan | /act      toggle plan/act mode")
	fmt.Println("  /new              start a fresh session")
	fmt.Println("  /resume           list, restore, or delete sessions")
	fmt.Println("  /save-config      write .gogen/gogen.md")
	fmt.Println("  verbose           toggle full tool output")

	if len(c.agent.Messages) > 0 {
		fmt.Printf("Session: %s (%d messages)\n", c.agent.SessionID, len(c.agent.Messages))
		printHistory(c.agent.Messages)
		if line := agent.FormatContextBrief(c.agent.ContextStats(ctx)); line != "" {
			fmt.Println(formatRightAlignedDimLine(line))
		}
	}

	for {
		input, err := readLine("\n> ", c.completeLine)
		if err != nil {
			if err == io.EOF {
				break
			}
			fmt.Printf("Input error: %v\n", err)
			break
		}
		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}
		if input == "exit" || input == "quit" {
			break
		}
		if input == "compact" || input == "/compact" {
			if err := c.agent.CompactHistory(ctx); err != nil {
				fmt.Printf("Compact failed: %v\n", err)
			} else {
				fmt.Printf("History compacted (%d messages remaining).\n", len(c.agent.Messages))
			}
			continue
		}
		if input == "verbose" || input == "/verbose" {
			c.verbose = !c.verbose
			state := "off"
			if c.verbose {
				state = "on"
			}
			fmt.Printf("Verbose tool output: %s\n", state)
			continue
		}
		if out, handled := c.agent.HandleModeCommand(input); handled {
			fmt.Println(out)
			continue
		}
		if out, handled := c.agent.HandleContextCommand(ctx, input); handled {
			fmt.Println(out)
			continue
		}
		if result, handled, err := c.agent.HandleSessionCommand(ctx, input, session.NewID()); handled {
			if err != nil {
				fmt.Printf("Session: %v\n", err)
			} else {
				fmt.Println(result.Output)
				if len(result.History) > 0 {
					printHistory(result.History)
				}
			}
			continue
		}
		if input == "/save-config" || input == "save-config" {
			if err := c.saveConfig(false); err != nil {
				fmt.Printf("Save config failed: %v\n", err)
			}
			continue
		}
		if out, handled, err := c.agent.HandleModelsCommand(ctx, input); handled {
			if err != nil {
				fmt.Printf("Models: %v\n", err)
			} else {
				fmt.Println(out)
			}
			continue
		}
		if strings.HasPrefix(input, "dir ") {
			newDir := strings.TrimSpace(strings.TrimPrefix(input, "dir "))
			absDir, err := filepath.Abs(newDir)
			if err != nil || !dirExists(absDir) {
				fmt.Printf("Error: directory does not exist: %s\n", newDir)
				continue
			}
			c.agent.SetWorkingDir(absDir)
			fmt.Printf("Changed working directory to: %s\n", absDir)
			continue
		}

		display := newStreamDisplay(c.verbose)
		handlers := display.handlers()

		turnMu.Lock()
		if turnCancel != nil {
			turnCancel()
		}
		turnCtx, tc := context.WithCancel(context.Background())
		turnCancel = tc
		turnMu.Unlock()

		_, streamErr := c.agent.StreamProcessInput(agent.ContextWithDeleteApprover(turnCtx, deleteApprover()), input, handlers)
		turnMu.Lock()
		turnCancel()
		turnCancel = nil
		turnMu.Unlock()
		fmt.Println()
		if streamErr != nil {
			if errors.Is(streamErr, context.Canceled) {
				fmt.Println("Cancelled.")
			} else {
				fmt.Printf("\nError: %v\n", streamErr)
			}
			continue
		}
		if line := agent.FormatContextBrief(c.agent.ContextStats(ctx)); line != "" {
			fmt.Println(formatRightAlignedDimLine(line))
		}
	}
}

func formatRightAlignedDimLine(text string) string {
	plain := text
	width := terminalColumns()
	if width > 0 && len(plain) < width {
		pad := width - len(plain)
		plain = strings.Repeat(" ", pad) + plain
	}
	// When text is wider than terminal, print as-is (left-aligned, no truncation).
	if os.Getenv("NO_COLOR") != "" {
		return plain
	}
	return "\x1b[2m" + plain + "\x1b[0m"
}

func (c *CLI) saveConfig(includeSecrets bool) error {
	if c.cfg == nil {
		return fmt.Errorf("config not available")
	}
	effective := *c.cfg
	effective.OpenAIModel = c.agent.CurrentModel()
	cfgPath := projectfile.DefaultSavePath(c.agent.WorkingDir)
	guidelinesPath := projectfile.DefaultGuidelinesSavePath(c.agent.WorkingDir)
	if err := projectfile.SaveConfig(cfgPath, guidelinesPath, &effective, c.agent.ProjectGuidelines, projectfile.WriteOptions{IncludeSecrets: includeSecrets}); err != nil {
		return err
	}
	fmt.Printf("Wrote config to %s\n", cfgPath)
	fmt.Printf("Wrote guidelines to %s\n", guidelinesPath)
	fmt.Println("Note: environment variables still override file values at runtime.")
	return nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

func printHistory(messages []llm.Message) {
	type hist struct {
		role    string
		content string
	}
	// Count total displayable messages so we know when there are
	// older ones not shown.
	total := 0
	for _, m := range messages {
		if (m.Role == "user" || m.Role == "assistant") && m.Content != "" {
			total++
		}
	}
	var keep []hist
	users, assts := 0, 0
	for i := len(messages) - 1; i >= 0 && (users < 2 || assts < 2); i-- {
		msg := messages[i]
		if msg.Role == "user" && msg.Content != "" && users < 2 {
			users++
			keep = append(keep, hist{role: "user", content: msg.Content})
		} else if msg.Role == "assistant" && msg.Content != "" && assts < 2 {
			assts++
			keep = append(keep, hist{role: "assistant", content: msg.Content})
		}
	}
	// Reverse back to original order.
	for i, j := 0, len(keep)-1; i < j; i, j = i+1, j-1 {
		keep[i], keep[j] = keep[j], keep[i]
	}

	truncated := total > len(keep)
	if truncated {
		fmt.Printf("\n⋮ (%d messages, showing last 4)", total)
	}
	styles := newCLIStyles()
	termWidth := terminalColumns()
	if termWidth < 40 {
		termWidth = 80
	}
	for _, h := range keep {
		if h.role == "assistant" {
			label := styles.wrap(styles.bold+styles.cyan, assistantLabel)
			wrapWidth := termWidth - len(assistantLabel) - 1
			if wrapWidth < 20 {
				wrapWidth = termWidth
			}
			wrapped := wordWrap(h.content, wrapWidth)
			indent := strings.Repeat(" ", len(assistantLabel)+1)
			for i, line := range strings.Split(wrapped, "\n") {
				contentStyled := styles.wrap(styles.dim, line)
				if i == 0 {
					fmt.Printf("\n%s %s", label, contentStyled)
				} else {
					fmt.Printf("\n%s %s", indent, contentStyled)
				}
			}
		} else {
			wrapped := wordWrap(h.content, termWidth)
			for _, line := range strings.Split(wrapped, "\n") {
				fmt.Printf("\n%s", line)
			}
		}
	}
	fmt.Println()
}
