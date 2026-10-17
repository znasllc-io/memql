---
title: Native editor host checks
audience: internal
status: stable
area: ops
sinceVersion: 0.29.5
owner: znas
---

# Native editor host checks

`scripts/ci/pipeline-editor-hosts.py` runs the existing real VS Code and browser
host tests without an Actions runner. It builds the shared SDKs, installs the
locked extension dependencies, stages a native language server and launches each
host. Unit tests and VSIX cross-compilation remain separate checks.

The Linux suite covers the core extension on VS Code 1.91.0 and stable, plus
Productivity on stable web and both desktop versions. The macOS Productivity
suite covers stable web and both desktop versions. A test compares these cases
with the retained Actions matrix while both definitions exist.

```sh
python3 scripts/ci/pipeline-editor-hosts.py \
  --platform linux/amd64 --output-dir .memql-editor-hosts

# On an authorized native Apple Silicon Cockpit worker:
python3 scripts/ci/pipeline-editor-hosts.py \
  --platform darwin/arm64 --suite productivity --output-dir .memql-editor-hosts
```

The requested platform must match the actual host. Intel macOS uses
`darwin/amd64`; emulation or cross-compilation is not host-test evidence. Native
Cockpit placement still requires the machine owner's pipeline consent. The
runner does not grant consent or install system packages.

On Linux, `deploy/toolchain-image/Dockerfile` includes the display and browser
runtime libraries, and its smoke test starts Xvfb as uid 1000. The locked npm
dependency installs its matching Chromium build. Rebuild and qualify the
toolchain through MemQL, publish its verified OCI artifact, then pin that digest
before wiring the host suite into the package. The older pinned image lacks the
required libraries; changing the Dockerfile alone does not upgrade it.

The release pipeline declares `build-tools.toolchain`, an isolated Linux AMD64
image build from `deploy/toolchain-image`. It runs only after preceding release
checks pass, with three CPUs, 8 GiB and a 45-minute deadline. It retains the OCI
archive and metadata in the Library and receives no publication credential.
Load that exact cloud-built archive into Docker and run
`deploy/toolchain-image/smoke-test.sh` against it before approving publication;
the earlier local build's smoke result does not qualify a different archive.
Publication and the subsequent reviewed digest pin remain separate steps.

Each attempt requires an empty output directory. Preparation failures stop
before any host starts. Once prepared, all selected hosts run even if one fails,
with a separate log and result for each. Any missing or failed host makes the
command fail. The summary records the source commit, tracked changes, actual
platform and requested VS Code versions; download logs record the resolved
stable release. Failed Productivity host diagnostics are copied before the next
lane replaces them. The enclosing pipeline worker enforces the deadline and
cancels the whole process group.

The core host suite's optional credentialed live-cluster cases are separate.
This runner clears ambient personal-cluster and executable overrides, so its
standard host pass must not be presented as a live-cluster connection pass.

Production matrix execution, the toolchain digest pin and actual macOS worker
evidence remain acceptance work under
[#5506](https://github.com/znasllc-io/memql/issues/5506) and
[#5509](https://github.com/znasllc-io/memql/issues/5509). A local image or a mocked
failure test is not that evidence.
