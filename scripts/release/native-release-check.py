#!/usr/bin/env python3
"""Bind release evidence to the repository's required App and release event."""
import datetime
import json
import sys


def timestamp(value):
    parsed = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise ValueError("timestamp lacks a timezone")
    return parsed


def policy(rules):
    bindings = set()
    for rule in rules:
        if rule.get("type") != "required_status_checks":
            continue
        for check in rule["parameters"]["required_status_checks"]:
            name = check.get("context", "")
            app = check.get("integration_id")
            if name.startswith("MemQL / ") and name.count(" / ") == 1:
                if not isinstance(app, int) or isinstance(app, bool) or app <= 0:
                    raise ValueError("native check is not bound to an App")
                if any(c in name for c in "\t\r\n"):
                    raise ValueError("invalid check name")
                bindings.add((name, app))
    if len(bindings) != 1:
        raise ValueError("expected one required native pipeline")
    name, app = bindings.pop()
    print(f"{name}\t{app}")


def run(pages, context, app, sha, published):
    since = timestamp(published)
    candidates = []
    for page in pages:
        for check in page["check_runs"]:
            if (check.get("name") != context + " / release"
                    or check.get("app", {}).get("id") != int(app)
                    or check.get("head_sha") != sha):
                continue
            started = check.get("started_at")
            if not started or timestamp(started) < since:
                continue
            if check.get("status") not in ("in_progress", "completed"):
                continue
            # An App check without the run identity/link cannot direct an
            # operator to the actual native build and is not sufficient proof.
            values = [check.get("external_id"), check.get("details_url"),
                      check.get("status"), check.get("conclusion") or ""]
            if not values[0] or not values[1]:
                continue
            if any(not isinstance(v, str) or any(c in v for c in "\t\r\n") for v in values):
                raise ValueError("invalid native run metadata")
            candidates.append((check["id"], values))
    if candidates:
        print("\t".join(max(candidates, key=lambda c: c[0])[1]))


def main():
    data = json.load(sys.stdin)
    if sys.argv[1] == "policy":
        policy(data)
    elif sys.argv[1] == "run":
        run(data, *sys.argv[2:])
    else:
        raise ValueError("unknown operation")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, TypeError, IndexError) as exc:
        print(f"native release evidence: {exc}", file=sys.stderr)
        sys.exit(2)
