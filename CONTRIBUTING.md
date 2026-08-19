# Contributing to memQL

Thanks for your interest in contributing.

memQL is pre-1.0. The DSL and engine API are still evolving, so the best contributions today are small, focused, and discussed before they land.

## Before you write code

1. **Open an issue first** for bugs, features, or design questions. There may already be a constraint or in-flight change that isn't obvious from the code.
2. **Every change goes through a branch + PR + the merge queue, regardless of size.** A repository ruleset refuses direct pushes to `main` (`pull_request` + `required_status_checks` + `merge_queue` are required), so even a one-line docs or typo fix needs a PR — there is no size-based exception. Open the PR, let CI go green, then enqueue it with `gh pr merge <n> --repo znasllc-io/memql` (bare form; it enqueues rather than merges immediately).
3. **For larger changes** — anything touching the engine, DSL grammar, or public wire surface — please discuss in an issue first. We don't want you to invest time in a direction we'd reject.

## Development setup

See [docs/public/overview/quickstart.md](docs/public/overview/quickstart.md) for the development environment (k3d + ArgoCD with PostgreSQL + TimescaleDB).

## Code style

- Go code must be `gofmt`-clean and pass `make vet` (bare `go vet` from the repo root only vets the root module's packages in this multi-module workspace, missing the other workspace modules — the same gotcha CLAUDE.md documents for the test command, memql#4032)
- Run `make test` locally before opening a PR — never a bare single-module `go test` sweep; see CLAUDE.md's Testing section for why
- One logical change per commit; commit messages explain *why*, not just *what*
- Match the surrounding code style and naming

## DSL contributions

- New DSL annotations or constructs require a design note in the issue first
- Reference files under `dsl/_reference/` are the source of truth for syntax; keep them in sync if behavior changes
- Lint any new or changed `.memql` files with `make dsl-lint` (runs `go run ./cmd/memqllint dsl/` in-repo, no separate binary needed) or, if you have it installed, the cockpit linter (`memql-cockpit lint dsl/`) — both catch structural issues

## License

By contributing, you agree that your contributions will be licensed under the Apache License 2.0.
