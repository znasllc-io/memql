# Contributing to memQL

Thanks for your interest in contributing.

memQL is pre-1.0. The DSL and engine API are still evolving, so the best contributions today are small, focused, and discussed before they land.

## Before you write code

1. **Open an issue first** for bugs, features, or design questions. There may already be a constraint or in-flight change that isn't obvious from the code.
2. **For documentation, typo, or small fixes**, a PR straight to `main` (or a short-lived branch) is welcome.
3. **For larger changes** — anything touching the engine, DSL grammar, or public wire surface — please discuss in an issue first. We don't want you to invest time in a direction we'd reject.

## Development setup

See [docs/public/overview/quickstart.md](docs/public/overview/quickstart.md) for the development environment (k3d + ArgoCD with PostgreSQL + TimescaleDB).

## Code style

- Go code must be `gofmt`-clean and pass `go vet ./...`
- Run `go test ./...` locally before opening a PR
- One logical change per commit; commit messages explain *why*, not just *what*
- Match the surrounding code style and naming

## DSL contributions

- New DSL annotations or constructs require a design note in the issue first
- Reference files under `dsl/_reference/` are the source of truth for syntax; keep them in sync if behavior changes
- The cockpit linter (`memql-cockpit lint dsl/`) catches structural issues — please run it on any new `.memql` files

## License

By contributing, you agree that your contributions will be licensed under the Apache License 2.0.
