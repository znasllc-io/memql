#!/usr/bin/env python3
"""Read the public GitHub facts used by repository audits, without credentials."""

import argparse
import json
import re
import sys
import urllib.error
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError("GitHub public audit reads do not follow redirects")


def read(path):
    request = urllib.request.Request(
        "https://api.github.com/" + path,
        headers={"Accept": "application/vnd.github+json", "User-Agent": "MemQL-public-audit"},
        method="GET",
    )
    # No gh configuration, credential helper, token or authenticated opener.
    opener = urllib.request.build_opener(NoRedirect())
    with opener.open(request, timeout=30) as response:
        raw = response.read(4 * 1024 * 1024 + 1)
    if len(raw) > 4 * 1024 * 1024:
        raise ValueError("GitHub public audit response exceeds 4 MiB")
    return json.loads(raw)


def name(value):
    if not isinstance(value, str) or not value or any(ord(c) < 32 for c in value):
        raise ValueError("GitHub public audit response contains an invalid name")
    return value


def facts(kind, repo, tag="", ruleset=""):
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo) or any(part in (".", "..") for part in repo.split("/")):
        raise ValueError("repository must be owner/name")
    root = "repos/" + repo
    if kind == "tags":
        result = []
        for page in range(1, 101):
            rows = read(f"{root}/tags?per_page=100&page={page}")
            if not isinstance(rows, list) or len(rows) > 100:
                raise ValueError("GitHub public tag response is not a bounded list")
            result.extend(name(row["name"]) for row in rows)
            if len(rows) < 100:
                return result
        raise ValueError("GitHub public tag inventory exceeds 10,000 tags")
    if kind == "release-assets":
        if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", tag):
            raise ValueError("release tag must be vX.Y.Z")
        release = read(f"{root}/releases/tags/{tag}")
        if release["tag_name"] != tag or release["draft"] is not False:
            raise ValueError("GitHub returned a different or unpublished release")
        if not isinstance(release["assets"], list):
            raise ValueError("GitHub release assets are not a list")
        return [name(asset["name"]) for asset in release["assets"]]
    if kind == "ruleset":
        if not re.fullmatch(r"[1-9][0-9]*", ruleset):
            raise ValueError("ruleset ID must be a positive integer")
        row = read(f"{root}/rulesets/{ruleset}")
        if row["id"] != int(ruleset) or row["enforcement"] not in ("active", "disabled", "evaluate"):
            raise ValueError("GitHub returned a different or invalid ruleset")
        if not isinstance(row["rules"], list):
            raise ValueError("GitHub ruleset rules are not a list")
        return [row["enforcement"], " ".join(sorted(name(rule["type"]) for rule in row["rules"]))]
    raise ValueError("unknown public audit read")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("kind", choices=("tags", "release-assets", "ruleset"))
    parser.add_argument("--repo", required=True)
    parser.add_argument("--tag", default="")
    parser.add_argument("--id", default="")
    args = parser.parse_args()
    try:
        rows = facts(args.kind, args.repo, args.tag, args.id)
    except (ValueError, KeyError, TypeError, OSError, urllib.error.URLError) as error:
        print(f"Could not read GitHub public audit data: {error}", file=sys.stderr)
        return 2
    if rows:
        print("\n".join(rows))
    return 0


if __name__ == "__main__":
    sys.exit(main())
