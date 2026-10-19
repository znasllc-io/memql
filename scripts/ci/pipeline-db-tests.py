#!/usr/bin/env python3
"""Run a native pipeline shard with the CI planner's per-package budgets.

The pipeline selects packages, owns sharding and provisions the database. This
command partitions that exact selection by the shared class table and gives
each class its own database in the disposable service, as CI does. It never
discovers additional packages or retries a test. Keeping package budgets
prevents the slower engine suite from granting every DB test the same cap.
"""
import json
import os
from pathlib import Path
import re
import subprocess
import sys
from urllib.parse import parse_qsl, unquote, urlsplit, urlunsplit
import uuid

import yaml


MODULE = "github.com/znasllc-io/memql/"
BUDGETS_PATH = Path(__file__).with_name("db-test-budgets.yaml")


def read_classes(budgets):
    classes = []
    for line in budgets["classes"].splitlines():
        words = line.split()
        if not words or words[0].startswith("#"):
            continue
        if len(words) != 6:
            raise ValueError("invalid class table row")
        lane, name, trees, timeout, mode, shards = words
        if lane != "db":
            continue
        if not re.fullmatch(r"[1-9][0-9]*s", timeout):
            raise ValueError("invalid test timeout")
        if mode != "-" and any(m not in ("serial", "uncached") for m in mode.split(",")):
            raise ValueError("unknown test mode")
        classes.append((name, [] if trees == "-" else trees.split(","), timeout))
    if sum(not c[1] for c in classes) != 1 or len({c[0] for c in classes}) != len(classes):
        raise ValueError("DB classes need unique names and exactly one default")
    return classes


def read_parallelism(budgets):
    value = str(budgets["databaseParallelism"])
    if not re.fullmatch(r"[1-9][0-9]*", value) or int(value) > 32:
        raise ValueError("DB test parallelism must be an integer from 1 to 32")
    return int(value)


def commands(packages, classes, parallelism):
    if not packages or len(set(packages)) != len(packages):
        raise ValueError("expected a nonempty, unique package selection")
    selected = {name: [] for name, _, _ in classes}
    default = next(name for name, trees, _ in classes if not trees)
    for package in packages:
        directory = package.removeprefix(MODULE).removeprefix("./")
        if not (package.startswith(MODULE) or package.startswith("./")) or not re.fullmatch(r"[A-Za-z0-9_-]+(?:/[A-Za-z0-9_.+-]+)*", directory) or any(p in (".", "..") for p in directory.split("/")):
            raise ValueError("expected explicit repository packages, without patterns or flags")
        matches = [(len(tree), name) for name, trees, _ in classes for tree in trees if directory == tree or directory.startswith(tree + "/")]
        name = max(matches)[1] if matches else default
        selected[name].append(package)
    # Every invocation is serial beside this shard's one Postgres, as before.
    # -count=1 also preserves the pipeline's always-uncached execution policy.
    # t.Parallel overlap must not silently halve when GOMAXPROCS follows a
    # smaller container CPU quota. Preserve the hosted lane's bounded overlap
    # without changing CPU reservations, package deadlines or test selection.
    return [(name, ["go", "test", "-count=1", "-p=1", "-parallel=" + str(parallelism), "-timeout=" + timeout, *selected[name]]) for name, _, timeout in classes if selected[name]]


def database_url(dsn, name):
    parts = urlsplit(dsn)
    # This command owns databases only inside the step's disposable service.
    # libpq query parameters can override URI routing, so admit only the
    # non-routing option used by the declared pipeline service.
    if (parts.scheme not in ("postgres", "postgresql") or
            parts.hostname not in ("localhost", "127.0.0.1", "::1") or
            not parts.username or not parts.path.strip("/") or parts.fragment or
            any(key != "sslmode" for key, _ in parse_qsl(parts.query, keep_blank_values=True))):
        raise ValueError("database isolation requires an explicit loopback service URI")
    return urlunsplit(parts._replace(path="/" + name))


def database_environment(dsn):
    parts = urlsplit(dsn)
    environment = dict(os.environ, MEMQL_DATABASE_DSN=dsn)
    # A hostaddr inherited from the operator would override the URI's host.
    for key in ("PGHOSTADDR", "PGSERVICE", "PGSERVICEFILE", "PGPASSWORD", "PGOPTIONS"):
        environment.pop(key, None)
    environment.update(PGHOST=parts.hostname, PGPORT=str(parts.port or 5432),
                       PGUSER=unquote(parts.username), PGDATABASE=unquote(parts.path[1:]),
                       PGSSLMODE=dict(parse_qsl(parts.query)).get("sslmode", "prefer"),
                       PGCONNECT_TIMEOUT="10")
    if parts.password is not None:
        environment["PGPASSWORD"] = unquote(parts.password)
    return environment


def sql(dsn, statement):
    # Keep connection credentials out of arguments and diagnostic output.
    environment = database_environment(dsn)
    environment["PGOPTIONS"] = "-c statement_timeout=30000"
    subprocess.run(["psql", "-X", "--no-password", "--quiet", "--set=ON_ERROR_STOP=1", "--command", statement],
                   env=environment, check=True, timeout=45)


def run(packages, classes, parallelism):
    selected = commands(packages, classes, parallelism)
    source = os.environ.get("MEMQL_DATABASE_DSN", "")
    database_url(source, "validate")
    for name, command in selected:
        print(json.dumps({"class": name, "command": command}), flush=True)
        # CI puts these classes in separate service databases. Native shards
        # may select several classes, but must not carry one suite's fixture
        # rows and planner statistics into the next suite's timeout budget.
        database = "memql_pipeline_" + uuid.uuid4().hex
        isolated = database_url(source, database)
        sql(source, "CREATE DATABASE " + database)
        try:
            sql(isolated, 'CREATE EXTENSION IF NOT EXISTS timescaledb CASCADE; '
                'CREATE EXTENSION IF NOT EXISTS "uuid-ossp"; '
                'CREATE EXTENSION IF NOT EXISTS pgcrypto; '
                'CREATE EXTENSION IF NOT EXISTS vector;')
            result = subprocess.run(command, env=database_environment(isolated), check=False)
        finally:
            # Only a database this invocation successfully created can reach
            # cleanup. No shared service database or previous run is dropped.
            sql(source, "DROP DATABASE " + database + " WITH (FORCE)")
        if result.returncode:
            return result.returncode if result.returncode > 0 else 1
    return 0


def main():
    if os.environ.get("MEMQL_REQUIRE_DB") != "1" or not os.environ.get("MEMQL_DATABASE_DSN"):
        raise ValueError("database-required execution and its DSN must be explicit")
    budgets = yaml.safe_load(BUDGETS_PATH.read_text())
    return run(os.environ.get("MEMQL_PACKAGES", "").split(), read_classes(budgets), read_parallelism(budgets))


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
        print("pipeline DB tests: " + str(error), file=sys.stderr)
        sys.exit(1)
