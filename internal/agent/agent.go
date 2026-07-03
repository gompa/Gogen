package agent

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

type Agent struct {
	Provider          llm.LLMProvider
	Executor          *Executor
	Context           *contextmgr.Manager
	Messages          []llm.Message
	WorkingDir        string
	Mode              Mode
	ProjectGuidelines string
	ProjectFilePath   string
	TestCommand       string
	LintCommand       string
	TodoManager       *TodoManager
	projectProfile    string
	MCPRegistry       MCPToolRegistry
	SessionStore      SessionPersister
	SessionID         string
	SessionLabel      string
	UsageAccum        UsageAccumulator
	PinManager        *PinManager
	lastTurnUsage     *llm.Usage
}

func NewAgent(provider llm.LLMProvider, executor *Executor, ctxMgr *contextmgr.Manager) *Agent {
	return &Agent{
		Provider:   provider,
		Executor:   executor,
		Context:    ctxMgr,
		Messages:   []llm.Message{},
		WorkingDir: executor.WorkingDir,
		Mode:       ModeAct,
	}
}

func (a *Agent) SetProjectContext(path, guidelines, testCommand, lintCommand string) {
	a.ProjectFilePath = path
	a.ProjectGuidelines = guidelines
	a.TestCommand = strings.TrimSpace(testCommand)
	a.LintCommand = strings.TrimSpace(lintCommand)
	a.projectProfile = ""
}

func (a *Agent) SetMCPRegistry(reg MCPToolRegistry) {
	a.MCPRegistry = reg
}

// pinLastUser pins the most recent user message so it survives compaction.
func (a *Agent) pinLastUser() error {
	if a.PinManager == nil {
		return fmt.Errorf("pin manager is not initialized")
	}
	a.PinManager.PinLastUser(a.Messages)
	return nil
}

func (a *Agent) listPins() string {
	if a.PinManager == nil {
		return "Pin manager is not initialized"
	}
	return a.PinManager.ListPins(a.Messages)
}

func (a *Agent) llmTools() []llm.Tool {
	tools := BuiltinTools()
	if a.MCPRegistry != nil {
		tools = append(tools, a.MCPRegistry.Definitions()...)
	}
	return tools
}

func (a *Agent) persistSession() {
	if a.SessionStore == nil || a.SessionID == "" {
		return
	}
	_ = a.SessionStore.Save(a.SessionID, SessionSnapshot{
		WorkingDir: a.WorkingDir,
		Model:      a.CurrentModel(),
		Mode:       a.Mode.String(),
		Label:      a.SessionLabel,
		Messages:   append([]llm.Message(nil), a.Messages...),
	})
}

// SetWorkingDir updates the agent and executor working directory together.
func (a *Agent) SetWorkingDir(dir string) {
	a.WorkingDir = dir
	a.projectProfile = ""
	if a.Executor != nil {
		a.Executor.WorkingDir = dir
	}
	a.persistSession()
}

func (a *Agent) ensureProjectProfile() string {
	if a.projectProfile != "" {
		return a.projectProfile
	}
	a.projectProfile = DetectProjectProfile(a.WorkingDir, a.TestCommand, a.LintCommand)
	return a.projectProfile
}

func finishStreamUI(h *llm.StreamHandlers) {
	if h != nil && h.OnStreamEnd != nil {
		h.OnStreamEnd()
	}
}

func ensureToolCallIDs(toolCalls []llm.ToolCall) {
	for j := range toolCalls {
		if toolCalls[j].ID == "" {
			toolCalls[j].ID = newToolCallID()
		}
	}
}

func newToolCallID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("call_%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (a *Agent) prepareMessages(ctx context.Context) []llm.Message {
	var view []llm.Message
	if a.Context == nil {
		view = a.Messages
	} else {
		a.Context.EnsureContextLimit(ctx)
		// Only compact at conversation boundaries (when the
		// most recent message is from the user).  Compacting
		// mid-tool-loop can drop assistant tool-call messages
		// whose results are still pending, confusing the LLM.
		if len(a.Messages) > 0 && a.Messages[len(a.Messages)-1].Role == "user" {
			if a.Context.ShouldCompact(a.Messages) {
				compacted, err := a.Context.Compact(ctx, a.Messages)
				if err == nil {
					a.Messages = compacted
				}
			}
		}
		view = a.Context.ViewForLLM(a.Messages)
	}
	view = withSystemPrompt(view, a.WorkingDir)
	return enrichSystemPrompt(view, a.WorkingDir, a.ProjectFilePath, a.ProjectGuidelines, a.ensureProjectProfile(), a.Mode)
}

// CompactHistory manually compacts conversation history at a task boundary.
func (a *Agent) CompactHistory(ctx context.Context) error {
	if a.Context == nil {
		return fmt.Errorf("context management is not configured")
	}
	if len(a.Messages) <= a.Context.Settings.KeepRecentMessages+1 {
		return fmt.Errorf("not enough history to compact (%d messages)", len(a.Messages))
	}
	compacted, err := a.Context.Compact(ctx, a.Messages)
	if err != nil {
		return err
	}
	a.Messages = compacted
	return nil
}

func formatToolError(result string, err error) string {
	if result == "" {
		return fmt.Sprintf("Error: %v", err)
	}
	return fmt.Sprintf("Error: %v\n\nOutput:\n%s", err, result)
}

func (a *Agent) appendToolResult(tc llm.ToolCall, result string) {
	a.Messages = append(a.Messages, llm.Message{
		Role:       "tool",
		Content:    result,
		ToolCallID: tc.ID,
	})
}

// StreamProcessInput streams tokens to the handlers as they arrive.
// It returns the final accumulated response or an error.
func (a *Agent) StreamProcessInput(ctx context.Context, input string, h *llm.StreamHandlers) (string, error) {
	a.Messages = append(a.Messages, llm.Message{Role: "user", Content: input})

	if err := a.requireModelSelected(ctx); err != nil {
		a.Messages = a.Messages[:len(a.Messages)-1]
		return "", err
	}

	if h == nil {
		h = &llm.StreamHandlers{}
	}

	for first := true; ; first = false {
		if ctx.Err() != nil {
			finishStreamUI(h)
			return "", ctx.Err()
		}
		view := a.prepareMessages(ctx)

		if first && h.OnStart != nil {
			h.OnStart()
		} else if !first && h.OnRoundStart != nil {
			h.OnRoundStart()
		}

		result, err := a.Provider.GenerateResponseStream(ctx, view, a.AllowedToolNames(), a.llmTools(), h)
		if err != nil {
			finishStreamUI(h)
			return "", err
		}
		a.recordTurnUsage(result.Usage)
		a.UsageAccum.Add(result.Usage)

		if len(result.ToolCalls) == 0 && result.Content != "" {
			finishStreamUI(h)
			a.Messages = append(a.Messages, llm.Message{Role: "assistant", Content: result.Content})
			a.persistSession()
			return result.Content, nil
		}

		if len(result.ToolCalls) == 0 && result.Content == "" {
			finishStreamUI(h)
			a.Messages = append(a.Messages, llm.Message{Role: "assistant", Content: ""})
			a.persistSession()
			return "", nil
		}

		if h.OnStreamEnd != nil {
			h.OnStreamEnd()
		}

		if result.PartialStream && h.OnRecoverPartialStream != nil {
			h.OnRecoverPartialStream()
		}

		ensureToolCallIDs(result.ToolCalls)

		a.Messages = append(a.Messages, llm.Message{
			Role:      "assistant",
			Content:   result.Content,
			ToolCalls: result.ToolCalls,
		})

		for _, tc := range result.ToolCalls {
			if ctx.Err() != nil {
				finishStreamUI(h)
				return "", ctx.Err()
			}
			if h.OnToolCall != nil {
				h.OnToolCall(tc)
			}
			if h.OnToolExecute != nil {
				h.OnToolExecute(tc.Name)
			}

			res, errTool := a.executeTool(ctx, tc)
			if ctx.Err() != nil {
				finishStreamUI(h)
				return "", ctx.Err()
			}
			success := errTool == nil
			if errTool != nil {
				res = formatToolError(res, errTool)
			}

			if h.OnToolResult != nil {
				h.OnToolResult(tc.ID, tc.Name, res, success)
			}

			a.appendToolResult(tc, res)
		}
		a.persistSession()
	}
}

func stringArg(args map[string]interface{}, key string) (string, error) {
	val, ok := args[key]
	if !ok {
		return "", fmt.Errorf("missing required argument %q", key)
	}
	s, ok := val.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string", key)
	}
	return s, nil
}

func stringArgOptional(args map[string]interface{}, key string) (string, error) {
	val, ok := args[key]
	if !ok {
		return "", nil
	}
	s, ok := val.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string", key)
	}
	return s, nil
}

func (a *Agent) toolContext(ctx context.Context) context.Context {
	if a.Executor != nil && !a.Executor.RequireDeleteApproval {
		ctx = ContextWithDeleteApprovalRequired(ctx, false)
	}
	return ctx
}

func boolArgOptional(args map[string]interface{}, key string) (bool, error) {
	val, ok := args[key]
	if !ok {
		return false, nil
	}
	b, ok := val.(bool)
	if !ok {
		return false, fmt.Errorf("argument %q must be a boolean", key)
	}
	return b, nil
}

func intArgOptional(args map[string]interface{}, key string) (int, error) {
	val, ok := args[key]
	if !ok {
		return 0, nil
	}
	switch v := val.(type) {
	case float64:
		if v != float64(int(v)) {
			return 0, fmt.Errorf("argument %q must be an integer", key)
		}
		return int(v), nil
	case int:
		return v, nil
	case int64:
		return int(v), nil
	default:
		return 0, fmt.Errorf("argument %q must be an integer", key)
	}
}

func stringSliceArg(args map[string]interface{}, key string) ([]string, error) {
	val, ok := args[key]
	if !ok {
		return nil, fmt.Errorf("missing required argument %q", key)
	}
	switch v := val.(type) {
	case []string:
		return v, nil
	case []interface{}:
		out := make([]string, 0, len(v))
		for i, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("argument %q[%d] must be a string", key, i)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("argument %q must be an array of strings", key)
	}
}

func (a *Agent) executeTool(ctx context.Context, tc llm.ToolCall) (string, error) {
	if err := a.checkPlanMode(tc.Name); err != nil {
		return "", err
	}
	if a.MCPRegistry != nil {
		if names := a.MCPRegistry.ToolNames(); names != nil {
			if _, ok := names[tc.Name]; ok {
				return a.MCPRegistry.CallTool(ctx, tc.Name, tc.Args)
			}
		}
	}
	ctx = a.toolContext(ctx)
	switch tc.Name {
	case "list_files":
		path, err := stringArg(tc.Args, "path")
		if err != nil {
			return "", err
		}
		recursive, _ := boolArgOptional(tc.Args, "recursive")
		tracked, _ := boolArgOptional(tc.Args, "tracked_only")
		return a.Executor.ListFiles(path, recursive, tracked)
	case "glob_files":
		pattern, err := stringArg(tc.Args, "pattern")
		if err != nil {
			return "", err
		}
		subpath, _ := stringArgOptional(tc.Args, "path")
		tracked, _ := boolArgOptional(tc.Args, "tracked_only")
		return a.Executor.GlobFiles(pattern, subpath, tracked)
	case "repo_overview":
		return a.Executor.RepoOverview()
	case "read_file":
		path, err := stringArg(tc.Args, "path")
		if err != nil {
			return "", err
		}
		offset, err := intArgOptional(tc.Args, "offset")
		if err != nil {
			return "", err
		}
		limit, err := intArgOptional(tc.Args, "limit")
		if err != nil {
			return "", err
		}
		search, _ := stringArgOptional(tc.Args, "search")
		return a.Executor.ReadFileRange(path, offset, limit, search)
	case "read_files":
		paths, err := stringSliceArg(tc.Args, "paths")
		if err != nil {
			return "", err
		}
		return a.Executor.ReadFiles(paths)
	case "write_file":
		path, err := stringArg(tc.Args, "path")
		if err != nil {
			return "", err
		}
		content, err := stringArg(tc.Args, "content")
		if err != nil {
			return "", err
		}
		err = a.Executor.WriteFile(path, content)
		if err != nil {
			return "", err
		}
		result := a.Executor.AppendSyntaxCheck("File written successfully", path)
		if diffOut, diffErr := runDiffQuick(a.WorkingDir, path); diffErr == nil && diffOut != "" {
			result += "\n\n" + diffOut
		}
		return result, nil
	case "execute_command":
		command, err := stringArg(tc.Args, "command")
		if err != nil {
			return "", err
		}
		return a.Executor.ExecuteCommand(ctx, command)
	case "replace_in_file":
		path, err := stringArg(tc.Args, "path")
		if err != nil {
			return "", err
		}
		search, err := stringArg(tc.Args, "search")
		if err != nil {
			return "", err
		}
		replace, err := stringArg(tc.Args, "replace")
		if err != nil {
			return "", err
		}
		replaceAll, err := boolArgOptional(tc.Args, "replace_all")
		if err != nil {
			return "", err
		}
		return a.Executor.ReplaceInFile(path, search, replace, replaceAll)
	case "patch_file":
		diff, err := stringArg(tc.Args, "diff")
		if err != nil {
			return "", err
		}
		dryRun, err := boolArgOptional(tc.Args, "dry_run")
		if err != nil {
			return "", err
		}
		fuzzy, err := boolArgOptional(tc.Args, "fuzzy")
		if err != nil {
			return "", err
		}
		return a.Executor.PatchFile(ctx, diff, dryRun, fuzzy)
	case "run_tests":
		target, _ := stringArgOptional(tc.Args, "target")
		extra, _ := stringArgOptional(tc.Args, "extra_args")
		return a.Executor.RunTests(ctx, target, extra, a.TestCommand)
	case "run_lint":
		extra, _ := stringArgOptional(tc.Args, "extra_args")
		return a.Executor.RunLint(ctx, extra, a.LintCommand)
	case "delete_file":
		path, err := stringArg(tc.Args, "path")
		if err != nil {
			return "", err
		}
		return a.Executor.DeleteFile(ctx, path)
	case "move_file":
		src, err := stringArg(tc.Args, "source")
		if err != nil {
			return "", err
		}
		dst, err := stringArg(tc.Args, "destination")
		if err != nil {
			return "", err
		}
		return a.Executor.MoveFile(src, dst)
	case "show_diff":
		subpath, _ := stringArgOptional(tc.Args, "path")
		staged, err := boolArgOptional(tc.Args, "staged")
		if err != nil {
			return "", err
		}
		return a.Executor.ShowDiff(ctx, subpath, staged)
	case "search_code":
		pattern, err := stringArg(tc.Args, "pattern")
		if err != nil {
			return "", err
		}
		subpath, _ := stringArgOptional(tc.Args, "path")
		glob, _ := stringArgOptional(tc.Args, "glob")
		contextLines, err := intArgOptional(tc.Args, "context_lines")
		if err != nil {
			return "", err
		}
		return a.Executor.SearchCode(ctx, pattern, subpath, glob, contextLines)
	case "find_references":
		symbol, err := stringArg(tc.Args, "symbol")
		if err != nil {
			return "", err
		}
		subpath, _ := stringArgOptional(tc.Args, "path")
		glob, _ := stringArgOptional(tc.Args, "glob")
		return a.Executor.FindReferences(ctx, symbol, subpath, glob)
	case "git_log":
		subpath, _ := stringArgOptional(tc.Args, "path")
		limit, err := intArgOptional(tc.Args, "limit")
		if err != nil {
			return "", err
		}
		return a.Executor.GitLog(ctx, subpath, limit)
	case "git_blame":
		path, err := stringArg(tc.Args, "path")
		if err != nil {
			return "", err
		}
		startLine, err := intArgOptional(tc.Args, "start_line")
		if err != nil {
			return "", err
		}
		limit, err := intArgOptional(tc.Args, "limit")
		if err != nil {
			return "", err
		}
		return a.Executor.GitBlame(ctx, path, startLine, limit)
	case "git_status":
		subpath, _ := stringArgOptional(tc.Args, "path")
		return a.Executor.GitStatus(ctx, subpath)
	case "git_commit":
		message, err := stringArg(tc.Args, "message")
		if err != nil {
			return "", err
		}
		return a.Executor.GitCommit(ctx, message)
	case "git_stage":
		paths, _ := stringSliceArg(tc.Args, "paths")
		return a.Executor.GitStage(ctx, paths)
	case "git_branch":
		name, _ := stringArgOptional(tc.Args, "name")
		create, _ := boolArgOptional(tc.Args, "create")
		return a.Executor.GitBranch(ctx, name, create)
	case "git_stash":
		message, _ := stringArgOptional(tc.Args, "message")
		pop, _ := boolArgOptional(tc.Args, "pop")
		return a.Executor.GitStash(ctx, message, pop)
	case "git_stash_list":
		return a.Executor.GitStashList(ctx)
	case "git_show":
		ref, _ := stringArgOptional(tc.Args, "ref")
		return a.Executor.GitDiffShow(ctx, ref)
	case "copy_file":
		src, err := stringArg(tc.Args, "source")
		if err != nil {
			return "", err
		}
		dst, err := stringArg(tc.Args, "destination")
		if err != nil {
			return "", err
		}
		return a.Executor.CopyFile(src, dst)
	case "todo_add":
		text, err := stringArg(tc.Args, "text")
		if err != nil {
			return "", err
		}
		if a.TodoManager == nil {
			return "", fmt.Errorf("todo manager is not initialized")
		}
		return a.TodoManager.AddTodo(text)
	case "todo_list":
		if a.TodoManager == nil {
			return "", fmt.Errorf("todo manager is not initialized")
		}
		return a.TodoManager.ListTodos(), nil
	case "todo_done":
		id, err := intArgOptional(tc.Args, "id")
		if err != nil || id == 0 {
			return "", fmt.Errorf("missing required argument %q", "id")
		}
		if a.TodoManager == nil {
			return "", fmt.Errorf("todo manager is not initialized")
		}
		return a.TodoManager.DoneTodo(id)
	case "todo_remove":
		id, err := intArgOptional(tc.Args, "id")
		if err != nil || id == 0 {
			return "", fmt.Errorf("missing required argument %q", "id")
		}
		if a.TodoManager == nil {
			return "", fmt.Errorf("todo manager is not initialized")
		}
		return a.TodoManager.RemoveTodo(id)
	case "todo_clear_done":
		if a.TodoManager == nil {
			return "", fmt.Errorf("todo manager is not initialized")
		}
		return a.TodoManager.ClearDoneTodos()
	case "list_definitions":
		path, err := stringArg(tc.Args, "path")
		if err != nil {
			return "", err
		}
		return a.Executor.ListDefinitions(path)
	case "web_search":
		query, err := stringArg(tc.Args, "query")
		if err != nil {
			return "", err
		}
		maxResults, err := intArgOptional(tc.Args, "max_results")
		if err != nil {
			return "", err
		}
		return a.Executor.WebSearch(ctx, query, maxResults)
	case "web_fetch":
		rawURL, err := stringArg(tc.Args, "url")
		if err != nil {
			return "", err
		}
		maxBytes, err := intArgOptional(tc.Args, "max_bytes")
		if err != nil {
			return "", err
		}
		return a.Executor.WebFetch(ctx, rawURL, maxBytes)
	case "find_file":
		name, err := stringArg(tc.Args, "name")
		if err != nil {
			return "", err
		}
		subpath, _ := stringArgOptional(tc.Args, "path")
		limit, _ := intArgOptional(tc.Args, "limit")
		return a.Executor.FindFile(name, subpath, limit)
	case "find_definition":
		symbol, err := stringArg(tc.Args, "symbol")
		if err != nil {
			return "", err
		}
		subpath, _ := stringArgOptional(tc.Args, "path")
		glob, _ := stringArgOptional(tc.Args, "glob")
		return a.Executor.FindDefinition(ctx, symbol, subpath, glob)
	case "session_rename":
		label, err := stringArg(tc.Args, "label")
		if err != nil {
			return "", err
		}
		return a.RenameSession(label)
	case "session_usage":
		return a.UsageAccum.Format(), nil
	case "context_pin_last":
		return "Pinned the last user message", a.pinLastUser()
	case "context_pins":
		return a.listPins(), nil
	default:
		return "", fmt.Errorf("unknown tool: %s", tc.Name)
	}
}
