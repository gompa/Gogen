package agent

import (
	"fmt"
	"strings"
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
context_pin_last, session_usage. See tool descriptions for details.

Guidelines:
Before editing: explore with repo_overview, search_code, list_definitions. Use read_file
offset/limit to avoid loading whole files. Batch reads with read_files.

Edits: surgical only — patch_file (fuzzy=true) or replace_in_file. Never rewrite a file,
delete+recreate, or patch that removes+re-adds everything. write_file is create-only.
If a patch fails, re-read and retry (don't rewrite). Run tests/linters after edits.

Safety: never exfiltrate secrets. No destructive commands (rm -rf, sudo, curl|bash).
For ambiguous tasks, state assumptions and proceed. Summarize changes when done.

Docs: verify claims against code (search for names you mention). Only document what
exists; do not invent config, CLI flags, or features. Omit unimplemented/roadmap items.`
}

// buildSystemView returns messages with the system prompt prepended (or the
// existing first system message enriched), folding the project profile,
// project rules, and plan-mode suffixes into the system message content in a
// SINGLE copy of the slice. The previous two-step pipeline
// (withSystemPrompt + enrichSystemPrompt) copied the whole message slice once
// per suffix; on long conversations that was 2-5 full shallow copies per turn
// and per ContextStats probe. The produced view is byte-identical to the old
// pipeline: the base system prompt, then the profile suffix, then the project
// rules header, then the plan-mode suffix, all on the leading system message.
func buildSystemView(messages []llm.Message, workingDir, projectFilePath, guidelines, projectProfile string, mode Mode) []llm.Message {
	if len(messages) == 0 {
		return messages
	}
	// Suffixes in the same order enrichSystemPrompt applied them.
	var suffix strings.Builder
	if projectProfile != "" {
		suffix.WriteString("\n\nProject profile (auto-detected):\n" + projectProfile)
	}
	if guidelines != "" {
		suffix.WriteString(projectRulesHeader(projectFilePath, guidelines))
	}
	if mode == ModePlan {
		suffix.WriteString(planModePromptSuffix)
	}

	// History already carries a system message (unusual — canonical history
	// is user/assistant/tool): keep the list and fold the suffixes into that
	// first system message, matching the old pipeline exactly.
	for i := range messages {
		if messages[i].Role == "system" {
			if suffix.Len() == 0 {
				return messages
			}
			out := append([]llm.Message(nil), messages...)
			out[i].Content += suffix.String()
			return out
		}
	}

	// No system message: prepend one with the full content in one copy.
	content := SystemPrompt(workingDir)
	content += suffix.String()
	out := make([]llm.Message, 0, len(messages)+1)
	out = append(out, llm.Message{Role: "system", Content: content})
	out = append(out, messages...)
	return out
}

func projectRulesHeader(path, guidelines string) string {
	name := path
	if name == "" {
		name = "project file"
	}
	return "\n\nProject rules (" + name + "):\n" + guidelines
}

const planModePromptSuffix = `

Plan mode is active. You may explore and explain only.
Do not call write, patch, replace, delete, move, lint, run_tests, execute_command, git_commit, git_stage, git_stash, or copy_file tools.
Produce a clear, actionable plan; the user will switch to act mode to implement.`
