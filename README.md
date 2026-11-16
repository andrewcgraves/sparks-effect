# Sparks Effect

One repository for the three Sparks Effect services.

| Directory  | What it is                  | Came from                                  |
| ---------- | --------------------------- | ------------------------------------------ |
| `api/`     | HTTP API (Go)               | `andrewcgraves/sparks-effect-api`          |
| `worker/`  | Routing worker (Go)         | `andrewcgraves/sparks-effect-routing-worker` |
| `website/` | Website (TypeScript)        | `andrewcgraves/sparks-effect-website`      |

Each directory was merged in with its full history. `git blame` reaches back to
the original commits, and so does `git log --follow -- <file>`. A plain
`git log -- api/` stops at the merge, because the files lived at the repository
root before it.

Deploy and monitoring configuration lives in a separate repository.
