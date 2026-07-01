package agent

import "strings"

// ResumeArgCompletions returns tab-completion candidates for the argument after resume / /resume.
func (a *Agent) ResumeArgCompletions(arg string) []string {
	if a.SessionStore == nil {
		return nil
	}
	arg = strings.TrimSpace(arg)

	if strings.HasPrefix(arg, "del ") || arg == "del" {
		partial := strings.TrimSpace(strings.TrimPrefix(arg, "del"))
		ids := filterPrefix(sessionIDs(a), partial)
		out := make([]string, len(ids))
		for i, id := range ids {
			out[i] = "del " + id
		}
		return out
	}

	var keywords []string
	if arg == "" || strings.HasPrefix("latest", arg) {
		keywords = append(keywords, "latest")
	}
	if arg == "" || strings.HasPrefix("del", arg) {
		keywords = append(keywords, "del")
	}

	ids := sessionIDs(a)
	if arg == "" {
		return append(keywords, ids...)
	}
	if matches := filterPrefix(append(keywords, ids...), arg); len(matches) > 0 {
		return matches
	}
	return filterPrefix(ids, arg)
}

func sessionIDs(a *Agent) []string {
	list, err := a.SessionStore.List(a.WorkingDir)
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(list))
	for _, s := range list {
		ids = append(ids, s.ID)
	}
	return ids
}

func filterPrefix(candidates []string, prefix string) []string {
	if prefix == "" {
		out := make([]string, len(candidates))
		copy(out, candidates)
		return out
	}
	var out []string
	for _, c := range candidates {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// LongestCommonPrefix returns the shared prefix of all values.
func LongestCommonPrefix(values []string) string {
	if len(values) == 0 {
		return ""
	}
	prefix := values[0]
	for _, v := range values[1:] {
		for len(prefix) > 0 && (len(v) < len(prefix) || v[:len(prefix)] != prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	return prefix
}

// ResumeLinePrefix returns the resume command prefix and argument when line is a resume command.
func ResumeLinePrefix(line string) (prefix, arg string, ok bool) {
	trimmed := strings.TrimRight(line, " \t")
	for _, p := range []string{"/resume ", "resume "} {
		if strings.HasPrefix(trimmed, p) {
			return p, strings.TrimSpace(trimmed[len(p):]), true
		}
	}
	return "", "", false
}
