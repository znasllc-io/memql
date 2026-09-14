# Changelog

The Marketplace renders this file on the extension's page, so it is written for
someone deciding whether to install rather than for someone reading the repo.

## 0.4.0

**The MemQL it speaks, and the cluster's**
- This release speaks MemQL edition 2026, grammar
  `2026.09-dsl-v1-foundations-7c878a05`: its
  highlighting, completion and diagnostics are that grammar's.
- When you connect to a cluster, the extension compares the cluster's MemQL
  with its own. A cluster on a newer grammar raises a notice naming the release
  of this extension to install, with a button that opens it in the Extensions
  view. A cluster on an older grammar raises a notice that completion may offer
  forms the cluster refuses until the cluster is updated. Each cluster is
  mentioned once per grammar in a session, and the details are in the MemQL
  Connection output channel.
- A cluster whose engine predates the comparison does not report its MemQL, and
  the extension says nothing about it.

**Writing MemQL**
- **The language line.** Each domain declares the language it is written in, in
  a `memql.toml` beside its `.memql` files. A domain without one is flagged on
  its files with a quick fix that writes it. Until then completion and hover
  are off for the workspace, and a notification says why.
- **The parse-time refusals.** Annotations are checked as you type against one
  registry of where each may be written and with what arguments. A misplaced,
  mis-argued or retired annotation is an error that names the fix.

## 0.3.1

First published release. The extension has been built and installed from source
throughout its development; this is the first version available from a registry.

**MemQL language support**
- Syntax highlighting, diagnostics, completion, hover and signature help for
  `.memql` files, from a language server bundled with the extension. It needs
  no cluster and no network.

**Local clusters**
- Create, repair, update and uninstall a local MemQL cluster from the
  Deployments panel, with a checklist before each run that says what will
  happen and a record of what did.
- Build node images from your own checkout and roll the cluster onto them.

**Connected clusters**
- Browse the constructs a cluster has loaded, run them, and read the results.

### Platforms

Published for `linux-x64`, `linux-arm64`, `darwin-arm64` and `darwin-x64`.

Language support works on all four. Creating a **local** cluster additionally
needs `linux/amd64` or `darwin/arm64` -- on the other two the installer says so
before it changes anything, and connecting to an existing cluster is unaffected.
