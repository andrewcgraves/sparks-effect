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
Production is not a git tag on this repo, and this repo has no `release.yml`.

## Linear

Linear Releases answer "which issues are on worker staging, and which made it
to production?" by scanning commits this repo already ships.

Two **continuous** pipelines, not one pipeline with stages. Staging auto-deploys
every commit on `main` while production lags on a kustomize pin, so the two
environments hold different commits at the same time:

| Pipeline | Created when | Version |
| --- | --- | --- |
| Worker Build Staging | `main` publishes `sha-<commit>` + `:staging` | short SHA |
| Worker production | `kustomize-config` pins that `sha-<commit>` in `overlays/production` ([SPA-267](https://linear.app/sparks-effect-personal/issue/SPA-267)) | recorded there, not here |

The action scans commits since the last release in **that** pipeline, pulls
`SPA-123` identifiers out of subjects and squash messages (`SPA-249: … (#12)`),
and attaches those issues to the new release. An issue that merged to `main`
shows up on Worker Build Staging immediately; it only appears on worker
production when the production pin moves.

Create **Worker Build Staging** in Linear (Settings → Releases, team Sparks
Effect), generate a **pipeline** access key (not a personal API key), and paste
it as this repository secret:

| name | kind | what it is |
| --- | --- | --- |
| `LINEAR_ACCESS_KEY_STAGING` | repository secret | Access key for the **Worker Build Staging** Linear release pipeline. |

If the key is missing, the Linear job skips with a notice. The image publish
still goes green. Worker production's key and YAML live in `kustomize-config`
([SPA-267](https://linear.app/sparks-effect-personal/issue/SPA-267)).
