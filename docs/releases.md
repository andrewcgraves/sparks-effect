# Branching and releases

One trunk: `main`.

- Branch from `main`. Open the pull request into `main`. Merge on green CI.
- No direct pushes to `main`.
- Staging follows `main` automatically. Production runs a build that someone
  promoted — never a branch merge, and never a rebuild.

## Images

Every commit on `main` publishes `sha-<commit>` (immutable) and `staging`
(moving).

No `latest`: the cluster pulled it, and Kubernetes defaults
`imagePullPolicy: Always` for `:latest`, so every pod restart was an unrecorded
production upgrade.

No `prd` branch either. Production runs whichever `sha-` tag `kustomize-config`
pins in `overlays/production`, and that pin is the record.

## Releasing

Bump the pin in `kustomize-config` — see that repo's `docs/releases.md`.
