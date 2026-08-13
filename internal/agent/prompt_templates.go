package agent

import (
	"strings"
	"sync"
)

// Configurable prompt templates for board-started agents and subagents.
//
// Each template is a plain string with named placeholders; an empty
// configured value — or a value EXACTLY equal to the built-in default —
// resolves to the built-in default. The default text therefore can never be
// baked into a config file ("saving the default" stores nothing) and future
// default improvements always reach users who did not customize. A value
// that merely differs (even by a word) is stored verbatim as a custom
// template; an OLD default text from a previous version no longer matches
// the current default and is treated as a pinned custom prompt.

// ResolvePromptTemplate returns the effective prompt template: the
// configured value, or the built-in default when unset OR when the value
// equals the default — a value equal to the default always means "the
// default", wherever it came from. The single implementation of this rule,
// shared by SystemPrompt, TicketPrompt, FormatSubagentJob, and the web
// settings push (server), so the semantics cannot drift between sites.
func ResolvePromptTemplate(configured, def string) string {
	if configured == "" || configured == def {
		return def
	}
	return configured
}

// NormalizePromptTemplate stores "" when a submitted prompt template equals
// the built-in default: "saving the default" must not bake the default text
// into the config file. Hand-edited files/env with the default text are
// still handled by ResolvePromptTemplate (same rule at resolution time).
func NormalizePromptTemplate(v, def string) string {
	if v == def {
		return ""
	}
	return v
}

// DefaultBoardStartPrompt is the built-in template for agents started from
// a board ticket. Placeholders: {id} {title} {description} {priority}
// {context}.
const DefaultBoardStartPrompt = `You have been assigned board ticket #{id}: {title}.

The ticket below describes work to do — it may be a bug, a feature
request, or a problem statement. Understand it, design and implement
the solution yourself, then verify your work before declaring it
complete.

{description}

Priority: {priority}

{context}

If anything is ambiguous, make a reasonable assumption and note it in
a comment on the ticket.

Use the board tool to keep the ticket updated as you work:
- comment on notable progress or blockers,
- move it to in_review when you believe it's done,
- mark it done (done {id}) once you've verified the work.

The ticket is already assigned to you and in progress — don't claim it again.`

// TicketPrompt renders the seed prompt for a board-started agent session.
// template is the user-configured template; empty (or equal to the built-in
// default) uses DefaultBoardStartPrompt. Substitution is single-pass over
// the template, so placeholder-like text inside the ticket's own fields is
// never re-substituted.
func TicketPrompt(item *BoardItem, template string) string {
	template = ResolvePromptTemplate(template, DefaultBoardStartPrompt)
	priority := item.Priority
	if priority == "" {
		priority = "none"
	}
	return strings.NewReplacer(
		"{id}", item.ID,
		"{title}", item.Title,
		"{description}", item.Description,
		"{priority}", priority,
		"{context}", activityContextBlock(item.Activity),
	).Replace(template)
}

// maxActivityContextLines caps the content-bearing entries included in a
// ticket prompt.
const maxActivityContextLines = 5

// maxActivityContextLineLen caps each included entry.
const maxActivityContextLineLen = 300

// activityContext returns the content-bearing activity entries (block
// reasons and comments), skipping the generated status-transition noise
// ("created", "claimed", "moved to …", "marked done"). The transition texts
// are stable: every mutation goes through appendActivityLocked in this
// package, so the prefix filter cannot drift.
func activityContext(activity []BoardActivity) []string {
	var out []string
	for _, act := range activity {
		text := strings.TrimSpace(act.Text)
		if text == "" {
			continue
		}
		switch {
		case text == "created", text == "claimed", text == "marked done":
			continue
		case strings.HasPrefix(text, "moved to "):
			continue
		}
		if len(text) > maxActivityContextLineLen {
			text = text[:maxActivityContextLineLen] + "…"
		}
		out = append(out, text)
	}
	if len(out) > maxActivityContextLines {
		out = append([]string(nil), out[len(out)-maxActivityContextLines:]...)
	}
	return out
}

// activityContextBlock renders the {context} placeholder value: a labeled
// list of the content-bearing entries, or "" when the log has none (a
// freshly started ticket keeps the prompt lean).
func activityContextBlock(activity []BoardActivity) string {
	entries := activityContext(activity)
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Ticket log context:\n")
	for _, e := range entries {
		b.WriteString("- " + e + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// DefaultSubagentPrompt is the built-in template wrapping subagent jobs.
// Placeholder: {job}.
const DefaultSubagentPrompt = `You are a subagent working on a task delegated by the parent agent.
Work independently and completely on the job below, then report back
with a concise summary of what you did and the outcome.

Job:
{job}`

// subagentPromptCfg holds the user-configured subagent job template
// ("" = the built-in default). Package state like ConfigureSystemPrompt.
var subagentPromptCfg struct {
	mu   sync.Mutex
	tmpl string
}

// ConfigureSubagentPrompt sets the user-configured subagent job template
// ("" restores the built-in default). The template may contain the {job}
// placeholder.
func ConfigureSubagentPrompt(tmpl string) {
	subagentPromptCfg.mu.Lock()
	subagentPromptCfg.tmpl = tmpl
	subagentPromptCfg.mu.Unlock()
}

func configuredSubagentPrompt() string {
	subagentPromptCfg.mu.Lock()
	defer subagentPromptCfg.mu.Unlock()
	return subagentPromptCfg.tmpl
}

// FormatSubagentJob wraps a subagent job with the configured template
// (empty, or equal to the built-in default → the built-in default) and
// substitutes the {job} placeholder. Callers (the web and TUI spawners)
// derive labels from the ORIGINAL job before wrapping, so sidebar labels
// and the subagent_started event keep the job the parent actually wrote.
func FormatSubagentJob(job string) string {
	tmpl := ResolvePromptTemplate(configuredSubagentPrompt(), DefaultSubagentPrompt)
	return strings.ReplaceAll(tmpl, "{job}", job)
}
