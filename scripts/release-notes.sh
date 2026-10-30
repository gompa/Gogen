#!/usr/bin/env bash
# Generate release notes from the commits in a release instead of GitHub's
# PR-based auto-generated notes.
#
# Usage: scripts/release-notes.sh [tag]
#
# Prints Markdown to stdout. Commits are grouped by the token before the first
# colon of the subject ("web:", "subagents:", "fix(steer):", "deps:"), listed
# oldest first, and linked to their commit page. Merge commits are skipped:
# their content is already carried by the commits they merge.
#
# The tag defaults to $GITHUB_REF_NAME (set when a workflow runs for a tag) and
# then to the tag that points at HEAD; the repository URL defaults to
# $GITHUB_SERVER_URL/$GITHUB_REPOSITORY and then to the origin remote, so the
# same script works in CI and locally:
#
#   ./scripts/release-notes.sh v0.0.23
set -eu

tag=${1:-${GITHUB_REF_NAME:-}}
if [ -z "$tag" ]; then
  tag=$(git describe --tags --exact-match 2>/dev/null || true)
fi
if [ -z "$tag" ]; then
  echo "release-notes: no tag given and HEAD is not tagged" >&2
  exit 1
fi
if ! git rev-parse --verify --quiet "refs/tags/$tag" >/dev/null; then
  echo "release-notes: tag '$tag' not found; fetch tags first" >&2
  exit 1
fi

previous=$(git describe --tags --abbrev=0 --match 'v*' "$tag^" 2>/dev/null || true)
if [ -n "$previous" ]; then
  range="$previous..$tag"
else
  range="$tag"
fi

repo_url="${GITHUB_SERVER_URL:-https://github.com}/${GITHUB_REPOSITORY:-}"
if [ -z "${GITHUB_REPOSITORY:-}" ]; then
  repo_url=$(git remote get-url origin 2>/dev/null || true)
  case "$repo_url" in
    git@*:*)
      repo_url="https://$(printf '%s' "$repo_url" | sed -E 's/^git@([^:]+):/\1\//')"
      ;;
    ssh://git@*)
      repo_url="https://$(printf '%s' "$repo_url" | sed -E 's#^ssh://git@([^/]+)/#\1/#')"
      ;;
  esac
fi
repo_url=${repo_url%.git}
repo_url=${repo_url%/}

printf "## What's Changed\n\n"

commits=$(git log --reverse --no-merges --format='%H%x09%s' "$range")
if [ -z "$commits" ]; then
  printf 'No changes.\n'
else
  printf '%s\n' "$commits" |
    awk -v repo="$repo_url" '
      # Section order: user-facing change types first, areas next, the rest last.
      function order_of(tok,   t) {
        t = tolower(tok)
        if (t == "feat" || t == "feature") return 10
        if (t == "fix" || t == "fixes" || t == "bugfix") return 20
        if (t == "perf") return 30
        if (t == "refactor") return 40
        return 100
      }

      function title(tok,   t, first) {
        t = tolower(tok)
        if (t == "feat" || t == "feature") return "Features"
        if (t == "fix" || t == "fixes" || t == "bugfix") return "Fixes"
        if (t == "perf") return "Performance"
        if (t == "refactor") return "Refactoring"
        if (t == "web" || t == "webui") return "Web UI"
        if (t == "tui") return "TUI"
        if (t == "llm") return "LLM"
        if (t == "mcp") return "MCP"
        if (t == "ci") return "CI"
        if (t == "vscode") return "VS Code"
        if (t == "docs" || t == "doc") return "Documentation"
        if (t == "deps" || t == "dep" || t == "dependencies") return "Dependencies"
        if (t == "test" || t == "tests") return "Tests"
        if (t == "chore") return "Chores"
        if (t == "build") return "Build"
        first = toupper(substr(tok, 1, 1))
        return first substr(tok, 2)
      }

      {
        # Split on the first tab only: the subject itself may contain one.
        split_at = index($0, "\t")
        sha = substr($0, 1, split_at - 1)
        subject = substr($0, split_at + 1)
        gsub(/\t/, " ", subject)

        order = 900
        section = "Other Changes"
        bullet = subject

        # "type:", "type(scope):", "type!:", or a bare area prefix ("web:").
        if (match(subject, /^[[:alpha:]][[:alnum:]_.-]*(\([^)]*\))?!?:/)) {
          head = substr(subject, 1, RLENGTH - 1)
          rest = substr(subject, RLENGTH + 1)
          sub(/^[[:space:]]+/, "", rest)

          tok = head
          scope = ""
          if (match(tok, /\([^)]*\)/)) {
            scope = substr(tok, RSTART + 1, RLENGTH - 2)
            tok = substr(tok, 1, RSTART - 1)
          }
          sub(/!$/, "", tok)

          if (rest != "") {
            order = order_of(tok)
            section = title(tok)
            bullet = (scope == "") ? rest : "**" scope ":** " rest
          }
        }

        short = substr(sha, 1, 7)
        if (repo == "")
          printf "%03d\t%s\t- %s (%s)\n", order, section, bullet, short
        else
          printf "%03d\t%s\t- %s ([%s](%s/commit/%s))\n", order, section, bullet, short, repo, sha
      }
    ' |
    sort -s -t $'\t' -k1,1n -k2,2 |
    awk -F '\t' '
      {
        if ($2 != section) {
          if (section != "") print ""
          section = $2
          printf "### %s\n\n", section
        }
        print $3
      }
    '
fi

if [ -n "$repo_url" ]; then
  printf '\n'
  if [ -n "$previous" ]; then
    printf '**Full Changelog**: %s/compare/%s...%s\n' "$repo_url" "$previous" "$tag"
  else
    printf '**Full Changelog**: %s/commits/%s\n' "$repo_url" "$tag"
  fi
fi
