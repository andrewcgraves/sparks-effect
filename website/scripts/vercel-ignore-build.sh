#!/usr/bin/env bash
# Vercel "Ignored Build Step" for the website. Vercel's convention is inverted
# from a normal check: exit 0 skips the build, exit 1 runs it.
#
# The website shares its repository with the API and the worker, so most commits
# change nothing here. This skips the build when nothing under website/ differs
# from the commit of the last deployment Vercel made for this branch
# (VERCEL_GIT_PREVIOUS_SHA). It builds whenever it cannot tell: no previous
# deployment, a previous commit missing from Vercel's shallow clone, or a git
# error.
#
# Vercel runs this with the project's Root Directory (website) as the working
# directory. The setting to paste is in docs/releases.md.

# The directory above this script is website/, wherever it was called from.
cd "$(dirname "$0")/.." || exit 1

previous="${VERCEL_GIT_PREVIOUS_SHA:-}"

if [[ -z "$previous" ]]; then
  echo "No previous deployment for this branch: building."
  exit 1
fi

if ! git cat-file -e "${previous}^{commit}" 2>/dev/null; then
  echo "Previous deployment's commit $previous is not in this clone: building."
  exit 1
fi

git diff --quiet "$previous" HEAD -- .
case $? in
  0)
    echo "Nothing under website/ changed since $previous: skipping the build."
    exit 0
    ;;
  1)
    echo "website/ changed since $previous: building."
    exit 1
    ;;
  *)
    echo "Could not compare against $previous: building."
    exit 1
    ;;
esac
