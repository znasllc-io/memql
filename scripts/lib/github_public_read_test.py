#!/usr/bin/env python3
import importlib.util
import io
import json
from pathlib import Path
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("public_read", Path(__file__).with_name("github-public-read.py"))
audit = importlib.util.module_from_spec(spec)
spec.loader.exec_module(audit)


class PublicReadTest(unittest.TestCase):
    def test_published_releases_read_every_page_before_returning(self):
        page = [{"tag_name": "v9.0.0", "draft": True, "prerelease": False,
                 "published_at": None}] * 100
        stable = {"tag_name": "v1.2.3", "draft": False, "prerelease": False,
                  "published_at": "2026-10-01T00:00:00Z"}
        with patch.object(audit, "read", side_effect=[page, [stable]]) as read:
            self.assertEqual(audit.facts("published-releases", "acme/widget"), ["v1.2.3"])
            self.assertEqual(read.call_args.args[0], "repos/acme/widget/releases?per_page=100&page=2")
        with patch.object(audit, "read", side_effect=[page, OSError("page unavailable")]):
            with self.assertRaises(OSError):
                audit.facts("published-releases", "acme/widget")

    def test_only_published_stable_releases_can_displace_live_docs(self):
        stable = {"tag_name": "v1.2.3", "draft": False, "prerelease": False,
                  "published_at": "2026-10-01T00:00:00Z"}
        rows = [{**stable, "tag_name": "v9.0.0", "draft": True},
                {**stable, "tag_name": "v8.0.0", "prerelease": True},
                {**stable, "tag_name": "v7.0.0", "published_at": None}, stable]
        with patch.object(audit, "read", return_value=rows):
            self.assertEqual(audit.facts("published-releases", "acme/widget"), ["v1.2.3"])

    def test_bounded_inventory_and_invalid_shapes_fail(self):
        stable = {"tag_name": "v1.2.3", "draft": False, "prerelease": False,
                  "published_at": "2026-10-01T00:00:00Z"}
        for response in ({"message": "rate limit"}, [{"unexpected": "name"}],
                         [{**stable, "tag_name": "bad\nname"}],
                         [{**stable, "draft": "false"}], [{**stable, "prerelease": None}],
                         [{**stable, "published_at": 123}]):
            with self.subTest(response=response), patch.object(audit, "read", return_value=response):
                with self.assertRaises((ValueError, KeyError)):
                    audit.facts("published-releases", "acme/widget")
        with patch.object(audit, "read", return_value=[stable] * 100):
            with self.assertRaisesRegex(ValueError, "10,000"):
                audit.facts("published-releases", "acme/widget")

    def test_release_identity_and_publication_are_checked(self):
        valid = {"tag_name": "v1.2.3", "draft": False, "prerelease": False, "published_at": "2026-10-01T00:00:00Z", "assets": [{"name": "docs-1.2.3.tgz"}]}
        with patch.object(audit, "read", return_value=valid):
            self.assertEqual(audit.facts("release-assets", "acme/widget", "v1.2.3"), ["docs-1.2.3.tgz"])
        for change in ({"tag_name": "v1.2.2"}, {"draft": True}, {"prerelease": True}, {"published_at": None}, {"assets": None}):
            with self.subTest(change=change), patch.object(audit, "read", return_value={**valid, **change}):
                with self.assertRaises(ValueError):
                    audit.facts("release-assets", "acme/widget", "v1.2.3")

    def test_ruleset_identity_and_missing_state_fail(self):
        valid = {"id": 123, "enforcement": "disabled", "rules": [{"type": "pull_request"}]}
        with patch.object(audit, "read", return_value=valid):
            self.assertEqual(audit.facts("ruleset", "acme/widget", ruleset="123"), ["disabled", "pull_request"])
        for change in ({"id": 124}, {"enforcement": None}, {"rules": None}):
            with self.subTest(change=change), patch.object(audit, "read", return_value={**valid, **change}):
                with self.assertRaises(ValueError):
                    audit.facts("ruleset", "acme/widget", ruleset="123")

    def test_paths_cannot_escape_the_declared_repository(self):
        with patch.object(audit, "read") as read:
            for repo in ("../..", "acme/..", "a/b/c", "acme/a?x=y", "https://example.test/a"):
                with self.subTest(repo=repo), self.assertRaises(ValueError):
                    audit.facts("published-releases", repo)
            for tag in ("../main", "v1.2.3?draft=true"):
                with self.subTest(tag=tag), self.assertRaises(ValueError):
                    audit.facts("release-assets", "acme/widget", tag)
            read.assert_not_called()

    def test_transport_is_get_without_credentials_or_redirects(self):
        class Opener:
            def open(inner, request, timeout):
                self.assertEqual(request.full_url, "https://api.github.com/repos/acme/widget/releases")
                self.assertEqual(request.get_method(), "GET")
                self.assertEqual(timeout, 30)
                self.assertFalse(request.has_header("Authorization"))
                self.assertFalse(request.has_header("Cookie"))
                return io.BytesIO(json.dumps([]).encode())
        with patch.dict("os.environ", {"GH_TOKEN": "must-not-travel", "GITHUB_TOKEN": "must-not-travel"}), patch.object(audit.urllib.request, "build_opener", return_value=Opener()) as build:
            self.assertEqual(audit.read("repos/acme/widget/releases"), [])
            self.assertIsInstance(build.call_args.args[0], audit.NoRedirect)
        with self.assertRaises(ValueError):
            audit.NoRedirect().redirect_request(None, None, 302, "", {}, "https://example.test/")


if __name__ == "__main__":
    unittest.main()
