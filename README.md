# Sparks Effect

One repository for the three Sparks Effect services.

| Directory  | What it is                  | Came from                                  |
| ---------- | --------------------------- | ------------------------------------------ |
| `api/`     | HTTP API (Go)               | `andrewcgraves/sparks-effect-api`          |
| `worker/`  | Routing worker (Go)         | `andrewcgraves/sparks-effect-routing-worker` |
| `website/` | Website (TypeScript)        | `andrewcgraves/sparks-effect-website`      |

Each directory was merged in with its full history, so `git log -- api/` and
`git blame` still reach back to the original commits.

Deploy and monitoring configuration lives in a separate repository.
