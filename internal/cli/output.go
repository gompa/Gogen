package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync/atomic"

	"gogen/internal/llm"
)

const (
	assistantLabel = "GoGen:"
	toolCallPrefix = "→"
	toolResultMark = "↳"
)

type cliStyles struct {
	enabled bool
	reset   string
	bold    string
	dim     string
	cyan    string
	yellow  string
	green   string
	red     string
}

func newCLIStyles() cliStyles {
	if os.Getenv("NO_COLOR") != "" {
		return cliStyles{}
	}
	return cliStyles{
		enabled: true,
		reset:   "\x1b[0m",
		bold:    "\x1b[1m",
		dim:     "\x1b[2m",
		cyan:    "\x1b[36m",
		yellow:  "\x1b[33m",
		green:   "\x1b[32m",
		red:     "\x1b[31m",
	}
}

func (s cliStyles) wrap(code, text string) string {
	if !s.enabled || code == "" {
		return text
	}
	return code + text + s.reset
}

type streamDisplay struct {
	styles  cliStyles
	verbose bool

	thinkingCleared  atomic.Bool
	thinkingOpen     atomic.Bool
	streamed         atomic.Bool
	assistantStarted atomic.Bool
	toolStreamed     atomic.Bool
}

func newStreamDisplay(verbose bool) *streamDisplay {
	return &streamDisplay{styles: newCLIStyles(), verbose: verbose}
}

func (d *streamDisplay) handlers() *llm.StreamHandlers {
	return &llm.StreamHandlers{
		OnStart:                d.onStart,
		OnRoundStart:           d.onRoundStart,
		OnThinkingToken:        d.onThinkingToken,
		OnToken:                d.onToken,
		OnStreamEnd:            d.onStreamEnd,
		OnToolCallStart:        d.onToolCallStart,
		OnToolCallArgsDelta:    d.onToolCallArgsDelta,
		OnToolCall:             d.onToolCall,
		OnToolExecute:          d.onToolExecute,
		OnRecoverPartialStream: d.onRecoverPartialStream,
		OnToolResult:           d.onToolResult,
	}
}

func (d *streamDisplay) flushOut() {
	_ = os.Stdout.Sync()
}

func (d *streamDisplay) clearThinking() {
	if d.thinkingCleared.CompareAndSwap(false, true) {
		fmt.Print("\r                    \r")
	}
}

func (d *streamDisplay) showThinking() {
	d.thinkingCleared.Store(false)
	fmt.Print(d.styles.wrap(d.styles.dim, "\n  ⋯ thinking"))
	d.flushOut()
}

func (d *streamDisplay) onStart() {
	d.streamed.Store(false)
	d.assistantStarted.Store(false)
	d.toolStreamed.Store(false)
	d.thinkingOpen.Store(false)
	d.showThinking()
}

func (d *streamDisplay) onRoundStart() {
	d.streamed.Store(false)
	d.assistantStarted.Store(false)
	d.toolStreamed.Store(false)
	d.thinkingOpen.Store(false)
	d.showThinking()
}

func (d *streamDisplay) beginThinkingBlock() {
	if d.thinkingOpen.CompareAndSwap(false, true) {
		d.clearThinking()
		if d.styles.enabled {
			fmt.Print("\n" + d.styles.dim + "<thinking>")
		} else {
			fmt.Print("\n<thinking>")
		}
		d.flushOut()
	}
}

func (d *streamDisplay) endThinkingBlock() {
	if !d.thinkingOpen.CompareAndSwap(true, false) {
		return
	}
	if d.styles.enabled {
		fmt.Print("</thinking>" + d.styles.reset + "\n")
	} else {
		fmt.Print("</thinking>\n")
	}
}

func (d *streamDisplay) onThinkingToken(token string) {
	if token == "" {
		return
	}
	d.beginThinkingBlock()
	fmt.Print(token)
	d.flushOut()
}

func (d *streamDisplay) beginAssistant() {
	if d.assistantStarted.CompareAndSwap(false, true) {
		label := d.styles.wrap(d.styles.bold+d.styles.cyan, assistantLabel)
		fmt.Print("\n" + label + " ")
	}
}

func (d *streamDisplay) onToken(token string) {
	d.clearThinking()
	d.endThinkingBlock()
	d.beginAssistant()
	d.streamed.Store(true)
	fmt.Print(token)
	d.flushOut()
}

func (d *streamDisplay) onStreamEnd() {
	d.endThinkingBlock()
	d.clearThinking()
	if d.streamed.Load() {
		fmt.Println()
		d.streamed.Store(false)
	}
	if d.toolStreamed.Load() {
		fmt.Println()
		d.toolStreamed.Store(false)
	}
	d.assistantStarted.Store(false)
}

func (d *streamDisplay) endAssistantLine() {
	if d.streamed.Load() {
		fmt.Println()
		d.streamed.Store(false)
	}
	d.assistantStarted.Store(false)
}

func (d *streamDisplay) onToolCallStart(_ int, _ string, name string) {
	d.endThinkingBlock()
	d.clearThinking()
	d.endAssistantLine()
	d.toolStreamed.Store(true)

	prefix := d.styles.wrap(d.styles.yellow, "  "+toolCallPrefix+" ")
	fmt.Printf("\n%s%s ", prefix, name)
	d.flushOut()
}

func (d *streamDisplay) onToolCallArgsDelta(_ int, _ string, _ string, argsDelta string) {
	if argsDelta == "" {
		return
	}
	fmt.Print(d.styles.wrap(d.styles.dim, argsDelta))
	d.flushOut()
}

func (d *streamDisplay) onRecoverPartialStream() {
	d.toolStreamed.Store(false)
}

func (d *streamDisplay) onToolExecute(name string) {
	fmt.Print(d.styles.wrap(d.styles.dim, "  ⋯ running "+name+"…\n"))
}

func (d *streamDisplay) onToolCall(tc llm.ToolCall) {
	if d.toolStreamed.Load() {
		return
	}

	d.endThinkingBlock()
	d.clearThinking()
	d.endAssistantLine()
	argStr := formatToolArgs(tc.Args)
	prefix := d.styles.wrap(d.styles.yellow, "  "+toolCallPrefix+" ")
	if argStr == "" {
		fmt.Printf("\n%s%s\n", prefix, tc.Name)
	} else {
		fmt.Printf("\n%s%s %s\n", prefix, tc.Name, d.styles.wrap(d.styles.dim, argStr))
	}
}

func (d *streamDisplay) onToolResult(_ string, name, result string, success bool) {
	status := "ok"
	statusStyle := d.styles.green
	if !success {
		status = "failed"
		statusStyle = d.styles.red
	}
	status = d.styles.wrap(statusStyle, status)

	mark := d.styles.wrap(d.styles.dim, "  "+toolResultMark+" ")
	if d.verbose {
		fmt.Printf("%s%s %s\n", mark, name, status)
		body := formatToolResultBody(result, 200, 5)
		for _, line := range strings.Split(body, "\n") {
			fmt.Println(d.styles.wrap(d.styles.dim, "  │ "+line))
		}
	} else {
		summary := summarizeToolResult(result, success)
		fmt.Printf("%s%s  %s  %s\n", mark, name, status, d.styles.wrap(d.styles.dim, summary))
	}
}

func formatToolArgs(args map[string]interface{}) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		val := fmt.Sprintf("%v", args[k])
		if len(val) > 80 {
			val = val[:77] + "..."
		}
		parts = append(parts, fmt.Sprintf("%s=%q", k, val))
	}
	return strings.Join(parts, " ")
}

func summarizeToolResult(result string, success bool) string {
	trimmed := strings.TrimSpace(result)
	if trimmed == "" {
		if success {
			return "(empty)"
		}
		return "(no output)"
	}
	lines := strings.Count(trimmed, "\n") + 1
	chars := len(trimmed)
	if !success {
		first := trimmed
		if idx := strings.Index(first, "\n"); idx >= 0 {
			first = first[:idx]
		}
		if len(first) > 120 {
			first = first[:117] + "..."
		}
		return fmt.Sprintf("%s (%d chars)", first, chars)
	}
	if lines == 1 && chars <= 120 {
		return trimmed
	}
	return fmt.Sprintf("(%d lines, %d chars)", lines, chars)
}

func formatToolResultBody(result string, maxChars, maxLines int) string {
	display := result
	if maxLines > 0 {
		parts := strings.SplitAfterN(display, "\n", maxLines)
		display = strings.Join(parts, "")
	}
	if maxChars > 0 && len(display) > maxChars {
		display = display[:maxChars] + "..."
	}
	if len(result) > len(display) {
		display += fmt.Sprintf(" (%d total chars)", len(result))
	}
	return display
}
