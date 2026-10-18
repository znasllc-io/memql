import importlib.util
import io
import json
from pathlib import Path
import struct
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import zipfile

spec = importlib.util.spec_from_file_location("release_assets", Path(__file__).with_name("pipeline-release-assets.py"))
assets = importlib.util.module_from_spec(spec)
spec.loader.exec_module(assets)


class ReleaseAssetsTests(unittest.TestCase):
    def test_pipeline_release_tag_is_normalized_for_docs(self):
        for tag in ("0.29.7", "v0.29.7"):
            self.assertEqual(assets.release_version(tag), "0.29.7")
        for tag in (None, "main", "vv0.29.7", "v0.29.7\ninjected"):
            with self.assertRaises(ValueError):
                assets.release_version(tag)

    def test_every_supported_install_platform_is_built(self):
        # The installable release contract survives removal of the unused
        # Marketplace workflow: all desktop platforms and the browser build.
        self.assertEqual(set(assets.TARGETS), {
            ("linux-x64", "linux", "amd64"),
            ("linux-arm64", "linux", "arm64"),
            ("darwin-x64", "darwin", "amd64"),
            ("darwin-arm64", "darwin", "arm64"),
            ("web", None, None),
        })

    def test_source_refusal_precedes_any_build_or_network(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "VERSION").write_text("0.29.7\n")
            with patch.object(assets, "run") as run:
                for repo, sha, version in (("../evil/repo", "a" * 40, "0.29.7"),
                                           ("acme/repo", "main", "0.29.7"),
                                           ("acme/repo", "a" * 40, "0.29.8"),
                                           ("acme/repo", "a" * 40, "0.29.7\ninjected")):
                    with self.assertRaises(ValueError):
                        assets.source(root, repo, sha, version)
                run.assert_not_called()
            with patch.object(assets, "run", return_value="b" * 40) as run:
                with self.assertRaises(ValueError):
                    assets.source(root, "acme/repo", "a" * 40, "0.29.7")
                self.assertEqual(run.call_count, 1)

    def test_cross_built_vsix_checks_actual_binary_and_web_isolation(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "test.vsix"

            def write(target, binary=b"", executable=True, native=True):
                with zipfile.ZipFile(path, "w") as z:
                    z.writestr("extension/package.json", json.dumps({"version": "0.3.0", "browser": "./out/browser.js"}))
                    z.writestr("extension.vsixmanifest", f'<PackageManifest><Metadata><Identity TargetPlatform="{target}"/></Metadata></PackageManifest>')
                    z.writestr("extension/out/browser.js", "// compiled")
                    if native:
                        info = zipfile.ZipInfo("extension/bin/linux-x64/memql-lsp")
                        info.external_attr = (0o100755 if executable else 0o100644) << 16
                        z.writestr(info, binary)

            header = bytearray(32)
            header[:6] = b"\x7fELF\x02\x01"
            struct.pack_into("<H", header, 18, 62)
            write("linux-x64", header)
            assets.verify_vsix(path, "linux-x64", "linux", "amd64", "0.3.0")
            struct.pack_into("<H", header, 18, 183)
            write("linux-x64", header)
            with self.assertRaisesRegex(ValueError, "architecture"):
                assets.verify_vsix(path, "linux-x64", "linux", "amd64", "0.3.0")
            write("web", header)
            with self.assertRaisesRegex(ValueError, "native"):
                assets.verify_vsix(path, "web", None, None, "0.3.0")
            write("web", native=False)
            assets.verify_vsix(path, "web", None, None, "0.3.0")

    def test_docs_metadata_cannot_be_a_link_or_another_version(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "docs.tgz"

            def write(version, symlink=False):
                with tarfile.open(path, "w:gz") as archive:
                    files = {"./manifest.json": json.dumps({"schema": 2, "version": version}),
                             "./memql-docs-version": version, "./llms.txt": "docs",
                             "./llms-full.txt": "docs", "./sitemap-docs.xml": "docs"}
                    for name, body in files.items():
                        item = tarfile.TarInfo(name)
                        item.size = len(body)
                        if symlink and name == "./manifest.json":
                            item.type, item.linkname = tarfile.SYMTYPE, "/outside"
                            item.size = 0
                        archive.addfile(item, io.BytesIO(body.encode()))

            write("0.29.7")
            assets.verify_docs(path, "0.29.7")
            write("0.29.6")
            with self.assertRaises(ValueError):
                assets.verify_docs(path, "0.29.7")
            write("0.29.7", symlink=True)
            with self.assertRaises(ValueError):
                assets.verify_docs(path, "0.29.7")


if __name__ == "__main__":
    unittest.main()
