#!/usr/bin/env bash
# PostToolUse hook for Edit and Write: gofmt the one Go file that was just
# edited. Every other file is left alone, and the hook always exits 0, so a
# missing formatter or a file that does not parse never fails the tool call.
#
# The website has no formatter of its own (only eslint), so website files are
# not touched here.

set -u

input=$(cat 2>/dev/null) || exit 0
command -v jq >/dev/null 2>&1 || exit 0

file=$(printf '%s' "$input" | jq -r '.tool_input.file_path // empty' 2>/dev/null) || exit 0
[ -n "$file" ] || exit 0

case "$file" in
	*.go) ;;
	*) exit 0 ;;
esac

case "$file" in
	*/testdata/* | */vendor/*) exit 0 ;;
esac

[ -f "$file" ] || exit 0

# Only files inside this project. Edit and Write always pass an absolute
# path; CLAUDE_PROJECT_DIR is set by Claude Code, cwd is the fallback.
root=${CLAUDE_PROJECT_DIR:-$(printf '%s' "$input" | jq -r '.cwd // empty' 2>/dev/null)}
[ -n "$root" ] || exit 0
case "$file" in
	"${root%/}"/*) ;;
	*) exit 0 ;;
esac

command -v gofmt >/dev/null 2>&1 || exit 0

# gofmt -w rewrites the file only when its formatting changes.
gofmt -w -- "$file" >/dev/null 2>&1
exit 0
