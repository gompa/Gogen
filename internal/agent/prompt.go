package agent

import (
	"fmt"
	"sync"

	"gogen/internal/llm"
)

// systemPromptTemplateOnce caches the template body once. Per-working-
// directory prompts are not cached: sprintf with a single %s is cheap, and
// long-lived processes that change working directories would otherwise grow an
// unbounded map.
var systemPromptTemplateOnce sync.Once
var systemPromptTmpl string

// SystemPrompt returns the default agent system prompt.
func SystemPrompt(workingDir string) string {
	systemPromptTemplateOnce.Do(func() {
		systemPromptTmpl = systemPromptTemplate()
	})
	return fmt.Sprintf(systemPromptTmpl, workingDir)
}

func systemPromptTemplate() string {
	return `You are GoGen, a coding agent working in the local repository at %s.

You have tools for: exploring files, searching code, editing files (prefer patch_file),
running tests/linters, git operations, web search, and task tracking.
Also: find_definition, find_references, rename_symbol, multi_edit, call_graph,
generate_test, context_pin_last, session_usage. See tool descriptions for details.

Guidelines:
Before editing: explore with repo_overview, search_code, list_definitions. Use read_file
offset/limit to avoid loading whole files. Batch reads with read_files.

Edits: prefer patch_file (fuzzy=true default; leave it on unless context matches exactly).
If patch fails, re-read and retry; write_file only as a last resort for small files.
Run tests/linters after edits; fix failures.

Safety: never exfiltrate secrets. No destructive commands (rm -rf, sudo, curl|bash).
For ambiguous tasks, state assumptions and proceed. Summarize changes when done.

Docs: verify claims against code (search for names you mention). Only document what
exists; do not invent config, CLI flags, or features. Omit unimplemented/roadmap items.`
}

func withSystemPrompt(messages []llm.Message, workingDir string) []llm.Message {
	if len(messages) == 0 {
		return messages
	}
	for _, msg := range messages {
		if msg.Role == "system" {
			return messages
		}
	}
	prompt := SystemPrompt(workingDir)
	out := make([]llm.Message, 0, len(messages)+1)
	out = append(out, llm.Message{Role: "system", Content: prompt})
	out = append(out, messages...)
	return out
}

func enrichSystemPrompt(messages []llm.Message, workingDir, projectFilePath, guidelines, projectProfile string, mode Mode) []llm.Message {
	out := messages
	if projectProfile != "" {
		out = appendToSystemPrompt(out, "\n\nProject profile (auto-detected):\n"+projectProfile)
	}
	if guidelines != "" {
		header := projectRulesHeader(projectFilePath, guidelines)
		out = appendToSystemPrompt(out, header)
	}
	if mode == ModePlan {
		out = appendToSystemPrompt(out, planModePromptSuffix)
	}
	return out
}

func projectRulesHeader(path, guidelines string) string {
	name := path
	if name == "" {
		name = "project file"
	}
	return "\n\nProject rules (" + name + "):\n" + guidelines
}

func appendToSystemPrompt(messages []llm.Message, suffix string) []llm.Message {
	if suffix == "" {
		return messages
	}
	out := make([]llm.Message, len(messages))
	copy(out, messages)
	for i, msg := range out {
		if msg.Role == "system" {
			out[i].Content = msg.Content + suffix
			return out
		}
	}
	return messages
}

const planModePromptSuffix = `

Plan mode is active. You may explore and explain only.
Do not call write, patch, replace, delete, move, lint, run_tests, execute_command, git_commit, git_stage, git_stash, or copy_file tools.
Produce a clear, actionable plan; the user will switch to act mode to implement.`
