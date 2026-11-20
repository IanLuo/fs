#!/usr/bin/env bash
#
# changed-needs-test.sh — fs's own test-adequacy question, for a review's tests slot.
#
#   scripts/changed-needs-test.sh <parent-commit>
#
# One question: does this change alter behaviour without altering a test? The change
# is <parent-commit>..HEAD — the round's diff, and in a review worktree HEAD is the
# commit under review.
#
#   exit 0  a test file changed with the code, or no non-test .go file changed
#           (docs, config and tests alone are not behaviour)
#   exit 1  a non-test .go file changed and no test file did: FAIL
#   exit 2  the question could not be asked (no argument, no such commit)
#
# The rule is co-location, not coverage. It is the cheapest test-adequacy check that
# catches the failure that happens — behaviour changed, tests untouched — it cannot
# be inflated like a percentage, and it needs no coverage tooling. Diff coverage on
# the changed lines replaces it once a round shows it was not enough; mutation on the
# diff stays a periodic audit, never a per-round gate.
#
# It prints one line, whichever way it answers, so the reviewer can paste it into the
# tests slot's --note.
set -euo pipefail

if [ "$#" -ne 1 ]; then
	echo "changed-needs-test: usage: $0 <parent-commit> (one line: the round's diff is <parent>..HEAD)" >&2
	exit 2
fi

parent=$1
if ! git rev-parse --verify --quiet --end-of-options "$parent^{commit}" >/dev/null; then
	echo "changed-needs-test: no commit '$parent' in this repository" >&2
	exit 2
fi

range="$parent..HEAD"
changed=$(git diff --name-only "$range" -- .)

code_files=""
test_files=""
while IFS= read -r path; do
	[ -n "$path" ] || continue
	case $path in
	*_test.go) test_files="${test_files:+$test_files, }$path" ;;
	*.go) code_files="${code_files:+$code_files, }$path" ;;
	esac
done <<<"$changed"

files() { printf '%s' "$1" | awk -F', ' '{print NF}'; }

if [ -n "$code_files" ] && [ -z "$test_files" ]; then
	echo "changed-needs-test $range: FAIL — $(files "$code_files") non-test .go file(s) changed and no *_test.go did: $code_files"
	exit 1
fi
if [ -n "$test_files" ]; then
	echo "changed-needs-test $range: PASS — a test file changed with the code: $test_files"
else
	echo "changed-needs-test $range: PASS — no non-test .go file changed"
fi
exit 0
