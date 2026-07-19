package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type patchFile struct {
	oldName string
	newName string
	hunks   []patchHunk
}

type patchHunk struct {
	oldStart int
	oldLines []string
	newLines []string
}

type patchPlan struct {
	target   string
	secure   string
	original []byte
	updated  string
	delete   bool
	create   bool
}

// PatchFile applies a unified diff to files under the working directory.
// When dryRun is true, patches are validated but not written.
// When fuzzy is true, hunks may be relocated when exact context no longer matches.
func (e *Executor) PatchFile(ctx context.Context, diff string, dryRun, fuzzy bool) (string, error) {
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("no patches found in diff")
	}

	var plans []patchPlan
	var okFiles []string
	var failFiles []string

	for _, pf := range files {
		plan, label, err := e.planPatch(pf, fuzzy)
		if err != nil {
			failFiles = append(failFiles, fmt.Sprintf("%s: %v", label, err))
			continue
		}
		plans = append(plans, plan)
		okFiles = append(okFiles, label)
	}

	if len(failFiles) > 0 {
		msg := formatPatchReport(okFiles, failFiles, dryRun)
		return msg, fmt.Errorf("patch failed for %d file(s)", len(failFiles))
	}

	if dryRun {
		return fmt.Sprintf("Dry run OK — would change %d file(s): %s\n\nNo files were modified.", len(plans), strings.Join(okFiles, ", ")), nil
	}

	var applied []string
	snapshots := make(map[string][]byte, len(plans))
	created := make([]string, 0)
	for _, plan := range plans {
		if plan.delete {
			if err := e.requireDeleteApproval(ctx, []string{plan.target}, "patch_file"); err != nil {
				rollbackPatches(snapshots, created)
				return "", err
			}
			if data, err := os.ReadFile(plan.secure); err == nil {
				snapshots[plan.secure] = data
			} else if !os.IsNotExist(err) {
				rollbackPatches(snapshots, created)
				return "", err
			}
			if err := os.Remove(plan.secure); err != nil && !os.IsNotExist(err) {
				rollbackPatches(snapshots, created)
				return "", err
			}
			applied = append(applied, plan.target+" (deleted)")
			continue
		}

		if !plan.create {
			data, err := os.ReadFile(plan.secure)
			if err != nil {
				rollbackPatches(snapshots, created)
				return "", err
			}
			snapshots[plan.secure] = data
		}

		if err := writeFileAtomic(plan.secure, []byte(plan.updated), 0o644); err != nil {
			rollbackPatches(snapshots, created)
			return "", err
		}
		if plan.create {
			created = append(created, plan.secure)
			applied = append(applied, plan.target+" (created)")
		} else {
			applied = append(applied, plan.target)
		}
	}

	msg := fmt.Sprintf("Applied patch to %d file(s): %s", len(applied), strings.Join(applied, ", "))
	return e.AppendSyntaxCheck(msg, appliedPaths(applied)...), nil
}

func appliedPaths(applied []string) []string {
	out := make([]string, 0, len(applied))
	for _, a := range applied {
		a = strings.TrimSuffix(a, " (deleted)")
		a = strings.TrimSuffix(a, " (created)")
		out = append(out, a)
	}
	return out
}

func rollbackPatches(snapshots map[string][]byte, created []string) {
	for _, path := range created {
		_ = os.Remove(path)
	}
	for path, data := range snapshots {
		_ = writeFileAtomic(path, data, 0o644)
	}
}

func formatPatchReport(ok, fail []string, dryRun bool) string {
	var b strings.Builder
	if dryRun {
		b.WriteString("Dry run failed.\n")
	} else {
		b.WriteString("Patch failed.\n")
	}
	if len(ok) > 0 {
		b.WriteString("OK: " + strings.Join(ok, ", ") + "\n")
	}
	if len(fail) > 0 {
		b.WriteString("FAILED:\n")
		for _, f := range fail {
			b.WriteString("  " + f + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func (e *Executor) planPatch(pf patchFile, fuzzy bool) (patchPlan, string, error) {
	target := pf.newName
	if target == "/dev/null" {
		target = pf.oldName
	}
	target = normalizePatchPath(target)
	if target == "" {
		return patchPlan{}, "", fmt.Errorf("could not determine target file from diff headers")
	}

	secure, err := e.securePath(target)
	if err != nil {
		return patchPlan{}, target, err
	}

	if pf.newName == "/dev/null" {
		return patchPlan{target: target, secure: secure, delete: true}, target + " (validated delete)", nil
	}

	isCreate := pf.oldName == "/dev/null"
	if !isCreate && len(pf.hunks) == 0 {
		return patchPlan{}, target, fmt.Errorf("no hunks found (check @@ headers use the form '@@ -start,count +start,count @@' with a space after @@)")
	}

	if isCreate {
		if _, err := os.Stat(secure); err == nil {
			return patchPlan{}, target, fmt.Errorf("file already exists; use a modify patch (--- a/%s) instead of creating from /dev/null", target)
		} else if !os.IsNotExist(err) {
			return patchPlan{}, target, err
		}
	}

	var original []string
	if !isCreate {
		data, err := os.ReadFile(secure)
		if err != nil {
			return patchPlan{}, target, fmt.Errorf("read: %w", err)
		}
		original = splitLinesPreserveTrailing(string(data))
	}

	updated, err := applyPatchHunks(original, pf.hunks, fuzzy)
	if err != nil {
		return patchPlan{}, target, err
	}

	label := target + " (validated)"
	if isCreate {
		label = target + " (validated create)"
	}

	return patchPlan{
		target:  target,
		secure:  secure,
		updated: joinLinesPreserveTrailing(updated),
		create:  isCreate,
	}, label, nil
}

func normalizePatchPath(name string) string {
	name = strings.TrimSpace(name)
	// Git diffs often append a tab + timestamp: "path\t2024-01-01 12:00:00.000000000 +0000"
	if i := strings.IndexByte(name, '\t'); i >= 0 {
		name = name[:i]
	}
	// Some tools use a space before an ISO-ish timestamp instead of a tab.
	if i := strings.IndexByte(name, ' '); i >= 0 {
		rest := strings.TrimSpace(name[i+1:])
		if looksLikePatchTimestamp(rest) {
			name = name[:i]
		}
	}
	name = strings.TrimSpace(name)
	if len(name) >= 2 {
		if (name[0] == '"' && name[len(name)-1] == '"') || (name[0] == '\'' && name[len(name)-1] == '\'') {
			name = name[1 : len(name)-1]
		}
	}
	name = strings.TrimPrefix(name, "a/")
	name = strings.TrimPrefix(name, "b/")
	return filepath.Clean(name)
}

func looksLikePatchTimestamp(s string) bool {
	if s == "" {
		return false
	}
	// ISO date / epoch-style: "2024-01-01 ..."
	if s[0] >= '0' && s[0] <= '9' {
		return true
	}
	lower := strings.ToLower(s)
	for _, day := range []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"} {
		if strings.HasPrefix(lower, day) {
			return true
		}
	}
	return false
}

func parseUnifiedDiff(diff string) ([]patchFile, error) {
	// Normalize line endings: handle CRLF (Windows) and bare CR (legacy Mac).
	diff = strings.ReplaceAll(diff, "\r\n", "\n")
	diff = strings.ReplaceAll(diff, "\r", "\n")
	lines := strings.Split(diff, "\n")
	// strings.Split leaves a trailing empty element for newline-terminated input;
	// that is not a hunk body line.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var files []patchFile
	var current *patchFile
	var hunk *patchHunk

	flushHunk := func() {
		if current != nil && hunk != nil && (len(hunk.oldLines) > 0 || len(hunk.newLines) > 0) {
			current.hunks = append(current.hunks, *hunk)
		}
		hunk = nil
	}
	flushFile := func() {
		flushHunk()
		if current != nil {
			files = append(files, *current)
			current = nil
		}
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		// Git multi-file diffs insert these between file sections.
		if strings.HasPrefix(line, "diff --git ") {
			flushFile()
			continue
		}
		if hunk == nil && (strings.HasPrefix(line, "index ") ||
			strings.HasPrefix(line, "old mode ") ||
			strings.HasPrefix(line, "new mode ") ||
			strings.HasPrefix(line, "new file mode ") ||
			strings.HasPrefix(line, "deleted file mode ") ||
			strings.HasPrefix(line, "similarity index ") ||
			strings.HasPrefix(line, "rename from ") ||
			strings.HasPrefix(line, "rename to ")) {
			continue
		}
		if strings.HasPrefix(line, "--- ") {
			flushFile()
			current = &patchFile{oldName: strings.TrimSpace(strings.TrimPrefix(line, "--- "))}
			continue
		}
		if strings.HasPrefix(line, "+++ ") {
			if current == nil {
				return nil, fmt.Errorf("malformed diff: +++ before ---")
			}
			current.newName = strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
			continue
		}
		if strings.HasPrefix(line, "@@") {
			flushHunk()
			if current == nil {
				return nil, fmt.Errorf("malformed diff: hunk before file header")
			}
			parsed, err := parseHunkHeader(line)
			if err != nil {
				return nil, err
			}
			hunk = &parsed
			continue
		}
		if hunk == nil {
			continue
		}
		if line == `\ No newline at end of file` {
			continue
		}
		// Empty lines in a hunk body are treated as empty context lines.
		// Unified diffs normally encode them as a single space (" "), but LLMs
		// often emit a bare blank line instead — dropping those corrupts patches.
		// Exception: blanks that only separate file sections must not become context.
		if len(line) == 0 {
			if isPatchFileBoundaryAhead(lines, i+1) {
				flushHunk()
				continue
			}
			hunk.oldLines = append(hunk.oldLines, "")
			hunk.newLines = append(hunk.newLines, "")
			continue
		}
		switch line[0] {
		case ' ':
			text := line[1:]
			hunk.oldLines = append(hunk.oldLines, text)
			hunk.newLines = append(hunk.newLines, text)
		case '-':
			hunk.oldLines = append(hunk.oldLines, line[1:])
		case '+':
			hunk.newLines = append(hunk.newLines, line[1:])
		default:
			return nil, fmt.Errorf("malformed hunk line: %q (context lines need a leading space)", line)
		}
	}
	flushFile()
	return files, nil
}

// isPatchFileBoundaryAhead reports whether the next non-empty line starts a new
// file section (or only trailing blanks remain). Used so blank separators between
// files are not absorbed as empty hunk context.
func isPatchFileBoundaryAhead(lines []string, from int) bool {
	for j := from; j < len(lines); j++ {
		if lines[j] == "" {
			continue
		}
		return strings.HasPrefix(lines[j], "--- ") || strings.HasPrefix(lines[j], "diff --git ")
	}
	return true
}

func parseHunkHeader(line string) (patchHunk, error) {
	// Accept both "@@ -1,4 +1,5 @@" and compacted "@@-1,4 +1,5@@".
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "@@"))
	rest = strings.TrimSuffix(rest, "@@")
	rest = strings.TrimSpace(rest)
	parts := strings.Fields(rest)
	if len(parts) < 2 {
		return patchHunk{}, fmt.Errorf("invalid hunk header: %q", line)
	}
	oldPart := strings.TrimPrefix(parts[0], "-")
	newPart := strings.TrimPrefix(parts[1], "+")
	if oldPart == parts[0] || newPart == parts[1] {
		return patchHunk{}, fmt.Errorf("invalid hunk header: %q", line)
	}
	oldStart, err := parseDiffLineCount(oldPart)
	if err != nil {
		return patchHunk{}, err
	}
	if _, err := parseDiffLineCount(newPart); err != nil {
		return patchHunk{}, err
	}
	return patchHunk{oldStart: oldStart}, nil
}

func parseDiffLineCount(part string) (int, error) {
	if part == "" {
		return 1, nil
	}
	num := part
	if idx := strings.IndexByte(part, ','); idx >= 0 {
		num = part[:idx]
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return 0, fmt.Errorf("invalid hunk line number %q", part)
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid hunk line number %q: must be non-negative", part)
	}
	return n, nil
}

func applyPatchHunks(original []string, hunks []patchHunk, fuzzy bool) ([]string, error) {
	out := append([]string(nil), original...)
	lineDelta := 0
	for hi, h := range hunks {
		start := h.oldStart - 1 + lineDelta
		if start < 0 {
			start = 0
		}
		n := len(h.oldLines)
		if n == 0 {
			if len(h.newLines) > 0 {
				if start > len(out) {
					start = len(out)
				}
				out = append(out[:start], append(append([]string(nil), h.newLines...), out[start:]...)...)
				lineDelta += len(h.newLines)
			}
			continue
		}
		inBounds := start <= len(out) && start+n <= len(out)
		if !inBounds && !fuzzy {
			if start > len(out) {
				return nil, fmt.Errorf("hunk %d/%d starts at line %d but file has %d lines", hi+1, len(hunks), h.oldStart, len(out)-lineDelta)
			}
			return nil, fmt.Errorf("hunk %d/%d extends past end of file (line %d)", hi+1, len(hunks), h.oldStart+n-1)
		}
		hint := start
		if hint > len(out) {
			hint = len(out)
		}
		var actual []string
		if inBounds {
			actual = out[start : start+n]
		} else if hint < len(out) {
			actual = out[hint:]
		}
		matched := findHunkMatch(out, h.oldLines, hint, n, fuzzy)
		if matched < 0 {
			return nil, formatHunkMismatch(hi+1, len(hunks), hint+1, actual, h.oldLines, fuzzy)
		}
		start = matched
		end := start + n
		replacement := append([]string(nil), h.newLines...)
		out = append(out[:start], append(replacement, out[end:]...)...)
		lineDelta += len(replacement) - n
	}
	return out, nil
}

// findHunkMatch locates oldLines within lines. Returns the start index, or -1
// if no match is found. When fuzzy is true, relocation and whitespace-tolerant
// matching are attempted before giving up.
func findHunkMatch(lines, oldLines []string, hint, n int, fuzzy bool) int {
	end := hint + n
	if end <= len(lines) && linesEqual(lines[hint:end], oldLines) {
		return hint
	}
	if !fuzzy {
		return -1
	}
	// Try exact relocation.
	if alt, ok := findHunkLocation(lines, oldLines, hint); ok {
		return alt
	}
	// Try whitespace-tolerant match at the current position.
	if end <= len(lines) && linesEqualFuzzy(lines[hint:end], oldLines) {
		return hint
	}
	// Try relocation with whitespace-tolerant matching.
	if alt, ok := findHunkLocationFuzzy(lines, oldLines, hint); ok {
		return alt
	}
	return -1
}

func formatHunkMismatch(hunkNum, hunkTotal, line int, actual, expected []string, fuzzy bool) error {
	firstDiff := 0
	for i := 0; i < len(actual) && i < len(expected); i++ {
		if actual[i] != expected[i] {
			firstDiff = i
			break
		}
	}
	msg := fmt.Sprintf("hunk %d/%d context mismatch at line %d", hunkNum, hunkTotal, line+firstDiff)
	if firstDiff < len(actual) && firstDiff < len(expected) {
		msg += fmt.Sprintf(": file has %q, patch expects %q", actual[firstDiff], expected[firstDiff])
	} else if len(actual) == 0 {
		msg += ": hunk context not found in file"
	}
	if !fuzzy {
		msg += " (fuzzy matching is disabled; re-read the file and regenerate the diff, or omit fuzzy=false)"
	} else {
		msg += " (re-read the file and regenerate the diff with exact current context)"
	}
	return fmt.Errorf("%s", msg)
}

func findHunkLocation(lines, oldLines []string, hint int) (int, bool) {
	n := len(oldLines)
	if n == 0 {
		return hint, true
	}
	var matches []int
	for i := 0; i <= len(lines)-n; i++ {
		if linesEqual(lines[i:i+n], oldLines) {
			matches = append(matches, i)
		}
	}
	switch len(matches) {
	case 0:
		return 0, false
	case 1:
		return matches[0], true
	default:
		best := matches[0]
		bestDist := absInt(matches[0] - hint)
		for _, m := range matches[1:] {
			if d := absInt(m - hint); d < bestDist {
				best = m
				bestDist = d
			}
		}
		return best, true
	}
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// linesEqualFuzzy is like linesEqual but normalises trailing whitespace
// on each line. This tolerates LLM-generated diffs that add spurious
// trailing spaces or tabs to context lines.
func linesEqualFuzzy(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.TrimRight(a[i], " \t\r") != strings.TrimRight(b[i], " \t\r") {
			return false
		}
	}
	return true
}

// findHunkLocationFuzzy is like findHunkLocation but uses
// whitespace-tolerant line comparison.
func findHunkLocationFuzzy(lines, oldLines []string, hint int) (int, bool) {
	n := len(oldLines)
	if n == 0 {
		return hint, true
	}
	var matches []int
	for i := 0; i <= len(lines)-n; i++ {
		if linesEqualFuzzy(lines[i:i+n], oldLines) {
			matches = append(matches, i)
		}
	}
	switch len(matches) {
	case 0:
		return 0, false
	case 1:
		return matches[0], true
	default:
		best := matches[0]
		bestDist := absInt(matches[0] - hint)
		for _, m := range matches[1:] {
			if d := absInt(m - hint); d < bestDist {
				best = m
				bestDist = d
			}
		}
		return best, true
	}
}

func linesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func splitLinesPreserveTrailing(s string) []string {
	if s == "" {
		return nil
	}
	// Normalize CRLF/CR on disk so patches with LF context still match Windows checkouts.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	return lines
}

func joinLinesPreserveTrailing(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}
