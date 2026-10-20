"""Bound freed GPU buffers in a managed MLX service and its spawned workers.

Enabled only by the service's PYTHONPATH and MEMQL_MLX_CACHE_LIMIT_BYTES.
Uses MLX's public allocator API; does not replace vendor modules or functions.
Python loads this hook in each fresh process, including multiprocessing spawn.
"""

import os
import sys


def configure(environ, import_core):
    value = environ.get("MEMQL_MLX_CACHE_LIMIT_BYTES")
    if value is None:
        return
    limit = int(value)
    if limit < 0 or limit > 4 * 1024**3:
        raise ValueError("MEMQL_MLX_CACHE_LIMIT_BYTES must be between 0 and 4294967296")
    core = import_core()
    core.set_cache_limit(limit)
    core.clear_cache()


def load_core():
    import mlx.core
    return mlx.core


try:
    configure(os.environ, load_core)
except Exception as error:
    # sitecustomize normally prints an exception and continues. A configured
    # memory guard must fail closed rather than launch without its bound.
    print(f"MemQL MLX memory initialization failed: {error}", file=sys.stderr, flush=True)
    os._exit(78)
