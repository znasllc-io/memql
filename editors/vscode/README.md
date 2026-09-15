# MemQL for VS Code

Write `.memql` where you write code. Inspect and run it where it lives.

The MemQL extension combines offline language support with an optional connection
to your MemQL clusters. It is part of the alpha MemQL platform.

## Install / update the extension locally

From a checkout of this repository:

```bash
make vscode-install
```

This packages the extension for your host and installs it through the `code` CLI.
Then run **Developer: Reload Window**. Requirements: VS Code 1.91+, Go using the
repository's `go.mod` toolchain, Node.js 20+, npm, Make, and unzip.
`make vscode-package` builds a VSIX without installing it.

For the full setup and alternate editor commands, see
[the installation guide](https://github.com/znasllc-io/memql/blob/main/docs/public/language/vscode.md#get-the-extension).
The publishing workflow targets Marketplace and Open VSX; a successful public
listing is not assumed by these instructions.

## Features

### Author offline

- Syntax highlighting and semantic tokens.
- Live diagnostics and cross-reference checks.
- Context-aware completion, hover, signature help, and go-to-definition.
- Snippet completions and edition rewrite code actions.

Open a folder of `.memql` files. The bundled `memql-lsp` server uses the same
MemQL Sense implementation as the engine. No cluster or credentials are needed
for local editing.

### Connect when you are ready

Open a trusted workspace and use **MemQL: Add Cluster**, then select the cluster
and sign in. The panel gives each job a place:

| View | Use it to |
|---|---|
| **Clusters** | Add, select, connect, and sign in to clusters |
| **Deployments** | Inspect the selected cluster's deployment history and supported lifecycle actions |
| **Constructs** | Browse definitions from the cluster or your workspace |
| **Data** | Browse rows your account is allowed to read |
| **Runs** | Reuse saved run configurations |

Runnable queries, mutations, logic, tools, and automations offer CodeLens actions
with argument forms. Running a mutation or tool can have real side effects.
Training indicators show whether the cluster knows the definition you are editing.

### Training: what the cluster knows about the file you are editing

Saving changes your local file. It does **not** train or deploy a construct.
Dry-run first, try supported definitions in a session, stage for personal use,
or explicitly promote to the cluster. Concepts cannot be privately staged;
seeded engine constructs require a rollout to change.

[Training guide](https://github.com/znasllc-io/memql/blob/main/docs/public/language/training.md)
explains the states, permissions, and effects of each action.

### MemQL OS and the editor

Use **Open Console** for the selected cluster's MemQL OS: the browser workspace
for Fleet, Files, Deployables, Nexus, and other apps. The editor owns source
files and cluster connections; OS apps organize work inside one cluster.

The local-cluster installer supports Linux x64 and Apple Silicon macOS. It needs
Docker running and shows its plan before changing your machine. Extension package
platforms and local-cluster installation support are separate.

## Start with a complete program

Follow [Your first MemQL program](https://github.com/znasllc-io/memql/blob/main/docs/public/language/first-program.md):
a caller-owned reading-list concept, a mutation that adds a row, a filtered query,
and a tool that exposes that query. Validate offline, then run against a development
cluster. The complete source is in `examples/reading-list/reading.memql`.

## Appearance

The extension provides MemQL light and dark editor themes and themed webviews.
Use VS Code's theme picker for the editor; see [appearance reference](REFERENCE.md#appearance)
for webview settings and native sidebar behavior.

## Reference and development

- [Detailed extension reference](REFERENCE.md) — lifecycle operations, training, data, security, and settings.
- [Runtime panel](https://github.com/znasllc-io/memql/blob/main/docs/public/language/vscode-runtime-panel.md).
- [Language server and packaging](https://github.com/znasllc-io/memql/blob/main/docs/public/language/vscode.md#architecture).
- [Landing page build and preview](site/README.md).

Contributors: run `make vscode-deps` before compiling from a clean checkout.
Use `make vscode-test` for extension unit tests and `make vscode-test-host` for
the Extension Development Host smoke lane. The
[detailed reference](REFERENCE.md#development) covers the development loop.
