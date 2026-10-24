# Contributing to memQL Cockpit

Thanks for your interest in contributing.

memQL Cockpit is pre-1.0 and tracks memQL core. The worker contract and DSL linter are still evolving, so the best contributions today are small, focused, and discussed before they land.

## Before you write code

1. **Open an issue first** for bugs, features, or UX changes. There may be a design constraint or in-flight change that isn't obvious from the code.
2. **For documentation, typo, or small fixes**, a PR straight to `main` (or a short-lived branch) is welcome.
3. **For larger changes** — new tabs, new worker modes, or anything that affects the gRPC contract with memQL core — please discuss in an issue first.

## Development setup

See the [README](README.md) for build and run instructions. The cockpit depends on `github.com/znasllc-io/memql`. During local development the `replace` directive in `go.mod` points at a sibling `../memql/` tree:

```
~/projects/
├── memql/
└── memql/
```

## Code style

- Go code must be `gofmt`-clean and pass `go vet ./...`
- Run `go test ./...` locally before opening a PR
- One logical change per commit; commit messages explain *why*, not just *what*
- Match the surrounding patterns of the package you are editing

## Computer-use variant

The computer-use build (`make cockpit-computeruse`) requires CGO + RobotGo (X11 deps on Linux, native frameworks on macOS). Most contributions only need the headless build (`make cockpit`); computer-use-specific changes should be tested on both supported platforms.

## License

By contributing, you agree that your contributions will be licensed under the Apache License 2.0.
