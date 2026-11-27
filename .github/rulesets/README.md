# Rulesets

Editing these files does not change the live rulesets. Re-import after a
change.

## main.json

Protects `main`: no deletion or force push, changes only through a pull
request (code-owner review, stale approvals dismissed, conversations
resolved), and the `ci-ok` check must pass. `ci-ok` is the aggregate job in
`.github/workflows/ci.yml`; `integration_id` 15368 is the GitHub Actions app.
Let `ci-ok` run once on `main` before importing, so GitHub knows the check.

Merge commits and squash are both allowed, and linear history is off, because
`scripts/import-from-old-repo.sh` brings the old repositories' history in as
merge commits.

GitHub does not let you approve your own pull request, so
`required_approving_review_count` is `0` while there is one maintainer. Set it
to `1` when there is a second reviewer.

## release-tags.json

Covers `api/v*`, `worker/v*` and `website/v*`. Nobody can create, move or
delete these tags except through the bypass for the repository admin role
(`actor_id` 5). A bare `v*` tag is not covered, and releases nothing.

## Importing

Settings, Rules, Rulesets, New ruleset, Import a ruleset, then pick the file.
Import `main.json` and `release-tags.json` separately. Afterwards check that
`main` requires `ci-ok`, and that the tag ruleset lists all three patterns.

## When the old repositories retire

Once nothing more is imported from them, in `main.json`:

- set `allowed_merge_methods` to `["squash"]`
- add `{ "type": "required_linear_history" }` to `rules`

Then re-import it over the existing `main` ruleset.
