# Memory admission for local inference

The production worker shares one host limiter across cluster connections and an
OS file lock across separate worker processes belonging to the same OS user.
The lock is independent of cluster configuration and `MEMQL_HOME`. Local
runtime calls serialize before starting GPU work, across model names and
modalities. Installed model ceilings still apply; their sum is no longer treated
as permission to start simultaneous loads against the same memory pool. Remote
endpoints do not consume this machine's inference gate.

Before switching models, the worker asks Ollama to unload idle runners it invoked
itself. It preserves a warm runner for another call to that same model and never
enumerates unrelated models to stop them. Ownership persists under the lock, so a replacement worker can reclaim a runner
started by its predecessor. Only local runtime identities are stored; credentials
are never recorded. The kernel releases the lock on process exit. Corrupt records
refuse admission. Models never invoked by Fleet remain included in the
available-memory measurement. This coordinates workers for one OS user, not
unrelated applications or other OS users; those are accounted for by live
headroom and pressure.

On macOS, admission counts free, purgeable and pageable file-backed memory, and
refuses warning/critical pressure. It keeps 1–4 GiB of additional headroom for the
OS. Ollama's reported weight bytes receive a conservative 25% plus 2 GiB allowance
for context and temporary buffers. This estimate is not a measured peak and
particularly large context windows may need a larger declared budget. An
OpenAI-compatible model should declare `peak_memory_bytes` in its `models` entry
because `/v1/models` cannot report its working set. The peak must include temporary
buffers, not just weights. Image models can also declare `memory_reference_pixels`
(1048576 for a measured 1024×1024 peak); larger output areas and batches scale the
reservation up, and smaller images never scale it down. Without a reference area,
the declared peak must cover the largest request accepted by the runtime. Ollama
context windows above 32K receive additional context headroom. Unknown peaks still
serialize and respect OS pressure but cannot predict
a cold load. Admission waits within the existing call deadline and continues
sending keepalives; it does not request a larger timeout.

## Managed MLX services

MLX's default freed-buffer cache can retain much more memory than the model's
active working set. Install `mlx-bootstrap/sitecustomize.py` into a dedicated
directory belonging to the managed runtime, and set these two variables only in
that service's environment:

```
PYTHONPATH=<absolute installed mlx-bootstrap directory>
MEMQL_MLX_CACHE_LIMIT_BYTES=1073741824
```

Preserve existing Python search paths if the service already declares any. Python
loads this standard initialization hook in both the server and spawned model
workers. It uses MLX's public allocator API to bound unused cached GPU buffers to
1 GiB; active tensors, image dimensions, weights and generation settings are
unchanged. Initialization failure stops the process instead of running without
the configured bound. This is a service-local setting, never a global shell
Python setting or a patch to the installed vendor package.

For on-demand servers, use a short idle unload interval (for example 3 seconds)
so completed image/audio models give memory back before the next text model
starts. This complements memory admission; shortening an idle timer alone does
not prevent an unsafe peak during generation. Drain requests before restarting
the service to apply its environment.

Regression checks run through `go test ./...`; the bootstrap tests use a fake MLX
allocator and do not load a model. Live validation must measure end-to-end time,
provider time, peak pressure, and delivery of the resulting file.
