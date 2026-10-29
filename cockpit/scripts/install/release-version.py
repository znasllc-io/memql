#!/usr/bin/env python3
"""Select the newest stable Cockpit release in the shared GitHub repository.

Engine releases share the catalog and can fill an entire page. Follow every
catalog page before choosing by semantic version: API order is publication
order, so publishing an older maintenance release must not downgrade latest.
Never fall back to the retired repository or to a Git tag without a published release.
"""

import argparse
import json
import os
import re
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from urllib.parse import urlparse

DEFAULT_API = "https://api.github.com/repos/znasllc-io/memql/releases?per_page=100"
STABLE_TAG = re.compile(r"cockpit/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")
MAX_PAGES = 100
MAX_PAGE_BYTES = 2 * 1024 * 1024
TOTAL_SECONDS = 120


class DiscoveryError(Exception):
    pass


def next_page(headers, initial_url):
    """A catalog response may advance only within that same catalog."""
    initial = urlparse(initial_url)
    links = []
    for line in headers.splitlines():
        if line.startswith("HTTP/"):
            links = []  # Redirect/interim responses cannot supply the final page.
        if line.lower().startswith("link:"):
            links.extend(re.split(r",(?=\s*<)", line.partition(":")[2]))
    found = None
    for link in links:
        match = re.fullmatch(r"\s*<([^<>]+)>(.*)", link)
        if not match:
            raise DiscoveryError("The release catalog returned invalid pagination metadata.")
        parameters = match.group(2)
        relation = None
        while parameters.strip():
            parameter = re.match(r'\s*;\s*([A-Za-z][A-Za-z0-9_-]*)\s*=\s*(?:"([^"\\]*)"|([^\s;,"\\]+))', parameters)
            if not parameter:
                raise DiscoveryError("The release catalog returned invalid pagination metadata.")
            if parameter.group(1).lower() == "rel":
                if relation is not None:
                    raise DiscoveryError("The release catalog returned invalid pagination metadata.")
                relation = parameter.group(2) if parameter.group(2) is not None else parameter.group(3)
            parameters = parameters[parameter.end():]
        if relation is None:
            raise DiscoveryError("The release catalog returned invalid pagination metadata.")
        if "next" not in relation.split():
            continue
        target = urlparse(match.group(1))
        if (target.scheme, target.netloc, target.path) != (initial.scheme, initial.netloc, initial.path) or target.fragment:
            raise DiscoveryError("The release catalog returned an invalid next page.")
        if found is not None:
            raise DiscoveryError("The release catalog returned more than one next page.")
        found = match.group(1)
    return found


def fetch_page(url, deadline):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise DiscoveryError("Reading the release catalog timed out. Retry or pin a published version.")
    # file:// is an offline fixture seam; HTTP needs the existing explicit
    # literal-loopback opt-in. Published discovery and redirects stay HTTPS.
    scheme = urlparse(url).scheme
    protocol = "=" + scheme
    with tempfile.TemporaryDirectory(prefix="memql-release-") as directory:
        headers = Path(directory) / "headers"
        try:
            response = subprocess.run([
                "curl", "--fail", "--silent", "--show-error", "--location",
                "--proto", protocol, "--proto-redir", protocol,
                "--max-redirs", "0" if scheme != "https" else "10",
                "--retry", "3", "--retry-delay", "2", "--connect-timeout", "15",
                "--max-time", str(min(60, max(1, int(remaining)))),
                "--max-filesize", str(MAX_PAGE_BYTES), "--dump-header", str(headers), url,
            ], capture_output=True, timeout=remaining, check=False)
        except subprocess.TimeoutExpired as error:
            raise DiscoveryError("Reading the release catalog timed out. Retry or pin a published version.") from error
        except OSError as error:
            raise DiscoveryError("curl is required to read the release catalog.") from error
        if response.returncode:
            raise DiscoveryError("Could not read the Cockpit release catalog. Check network access or GitHub rate limits, then retry.")
        if len(response.stdout) > MAX_PAGE_BYTES:
            raise DiscoveryError("The release catalog response is too large.")
        try:
            rows = json.loads(response.stdout)
        except (ValueError, UnicodeError) as error:
            raise DiscoveryError("The release catalog returned invalid JSON. Retry later.") from error
        if not isinstance(rows, list) or any(not isinstance(row, dict) for row in rows):
            raise DiscoveryError("The release catalog returned an invalid release list. Retry later.")
        return rows, headers.read_text(errors="replace") if headers.exists() else ""


def resolve(api_url):
    parsed = urlparse(api_url)
    loopback = (os.environ.get("MEMQL_INSTALL_ALLOW_LOOPBACK_HTTP") == "1" and
                parsed.scheme == "http" and parsed.hostname in ("127.0.0.1", "::1"))
    if (parsed.scheme not in ("https", "file") and not loopback) or parsed.username or parsed.password or parsed.fragment:
        raise DiscoveryError("The release catalog must use HTTPS.")
    deadline = time.monotonic() + TOTAL_SECONDS
    seen = set()
    best = None
    url = api_url
    while url:
        if url in seen or len(seen) >= MAX_PAGES:
            raise DiscoveryError("The release catalog could not be read completely. Retry or pin a published version.")
        seen.add(url)
        rows, headers = fetch_page(url, deadline)
        for row in rows:
            tag = row.get("tag_name")
            match = STABLE_TAG.fullmatch(tag) if isinstance(tag, str) and len(tag) <= 100 else None
            if row.get("draft") is not False or row.get("prerelease") is not False or not match:
                continue
            version = tuple(int(part) for part in match.groups())
            if best is None or version > best[0]:
                best = (version, tag)
        url = next_page(headers, api_url)
    if best is None:
        raise DiscoveryError("No stable Cockpit release has been published in the MemQL repository. Nothing was installed.")
    return best[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--api-url", default=DEFAULT_API)
    args = parser.parse_args()
    try:
        print(resolve(args.api_url))
    except DiscoveryError as error:
        print("ERROR: " + str(error), file=sys.stderr)
        return 4
    return 0


if __name__ == "__main__":
    sys.exit(main())
