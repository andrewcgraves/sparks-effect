#!/usr/bin/env bash
# Merge a branch of one of the three pre-monorepo repositories into its
# directory here, keeping history.
#
# Use it for two things while the old repositories still exist:
#   - catching up with commits that landed on an old default branch after the
#     import (run it on main, or on a branch you then open a PR from);
#   - carrying an unfinished feature branch across (run it on a new branch).
#
#   scripts/import-from-old-repo.sh api main
#   scripts/import-from-old-repo.sh website andrew/spa-497-some-branch
#
# The merge is a normal three-way merge shifted into the service directory, so
# a file changed both there and here conflicts in the usual way. The old
# repository's .github/ no longer exists here: a change to a workflow there
# shows up as a modify/delete conflict and has to be redone by hand in the
# root .github/workflows/.
set -euo pipefail

usage() {
  echo "usage: $0 <api|worker|website> <branch-or-ref>" >&2
  exit 2
}

[ $# -eq 2 ] || usage
service=$1
ref=$2

case "$service" in
  api)     repo=sparks-effect-api ;;
  worker)  repo=sparks-effect-routing-worker ;;
  website) repo=sparks-effect-website ;;
  *)       usage ;;
esac

cd "$(git rev-parse --show-toplevel)"

if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "error: commit or stash your changes first" >&2
  exit 1
fi

url="git@github.com:andrewcgraves/${repo}.git"
git fetch --no-tags "$url" "$ref"
sha=$(git rev-parse --short FETCH_HEAD)

if git merge-base --is-ancestor FETCH_HEAD HEAD; then
  echo "${repo}@${sha} (${ref}) is already merged into ${service}/"
  exit 0
fi

git merge --no-ff -X "subtree=${service}" \
  -m "chore(${service}): merge ${repo}@${sha} (${ref}) into ${service}/" \
  FETCH_HEAD
