import copy
import gzip
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import tarfile
import tempfile
import unittest
import subprocess
import sys

spec = importlib.util.spec_from_file_location("oci_verify", Path(__file__).with_name("verify-oci.py"))
oci = importlib.util.module_from_spec(spec)
spec.loader.exec_module(oci)


def sha(value):
    return "sha256:" + hashlib.sha256(value).hexdigest()


class OCIIntegrityTest(unittest.TestCase):
    def fixture(self, fault=""):
        files = {"oci-layout": b'{"imageLayoutVersion":"1.0.0"}'}

        def blob(value, media):
            data = json.dumps(value).encode() if isinstance(value, dict) else value
            digest = sha(data)
            files["blobs/sha256/" + digest.split(":")[1]] = data
            return {"mediaType": media, "digest": digest, "size": len(data)}

        layer_bytes = b"image filesystem layer fixture" * 500
        compressed = gzip.compress(layer_bytes)
        layer = blob(compressed, oci.LAYER + "+gzip")
        config = {"os": "linux", "architecture": "arm64", "rootfs": {"type": "layers", "diff_ids": [sha(layer_bytes)]}}
        if fault == "platform":
            config["architecture"] = "amd64"
        if fault == "variant":
            config["variant"] = "v9"
        if fault == "diff-id":
            config["rootfs"]["diff_ids"] = [sha(b"other layer")]
        manifest = {"schemaVersion": 2, "mediaType": oci.MANIFEST, "config": blob(config, oci.CONFIG), "layers": [layer]}
        if fault == "descriptor-size":
            layer["size"] += 1
        if fault == "external":
            layer["urls"] = ["https://example.invalid/must-not-fetch"]
        if fault == "embedded":
            layer["data"] = "secret"
        if fault == "missing":
            del files["blobs/sha256/" + layer["digest"].split(":")[1]]
        image = blob(manifest, oci.MANIFEST)
        image["platform"] = {"os": "linux", "architecture": "arm64"}
        index = {"schemaVersion": 2, "mediaType": oci.INDEX, "manifests": [image]}
        if fault == "schema-type":
            index["schemaVersion"] = 2.0
        if fault == "index-variant":
            image["platform"]["variant"] = "v9"
        if fault == "multiple":
            index["manifests"].append(copy.deepcopy(image))
        files["index.json"] = json.dumps(index).encode()
        if fault == "corrupt":
            files["blobs/sha256/" + layer["digest"].split(":")[1]] = compressed[:-1] + bytes([compressed[-1] ^ 1])
        if fault == "duplicate-json":
            files["oci-layout"] = b'{"imageLayoutVersion":"1.0.0","imageLayoutVersion":"1.0.0"}'
        if fault == "non-json":
            files["oci-layout"] = b'{"imageLayoutVersion":"1.0.0","extra":NaN}'
        if fault == "traversal":
            files["../outside"] = b"never extracted"
        archive = io.BytesIO()
        with tarfile.open(fileobj=archive, mode="w", format=tarfile.USTAR_FORMAT) as tar:
            for name, data in files.items():
                info = tarfile.TarInfo(name)
                info.size = len(data)
                tar.addfile(info, io.BytesIO(data))
            if fault == "duplicate-entry":
                info = tarfile.TarInfo("index.json")
                tar.addfile(info, io.BytesIO())
            if fault == "symlink":
                info = tarfile.TarInfo("link")
                info.type, info.linkname = tarfile.SYMTYPE, "/etc/passwd"
                tar.addfile(info)
        return archive.getvalue(), image["digest"], len(layer_bytes)

    def verify(self, payload, **kwargs):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "image.tar"
            path.write_bytes(payload)
            return oci.verify(path, "linux/arm64", kwargs.pop("max_bytes", 1024**2), kwargs.pop("max_unpacked_bytes", 1024**2), **kwargs)

    def test_real_hashes_and_uncompressed_diffid(self):
        payload, digest, decoded = self.fixture()
        result = self.verify(payload, expected_digest=digest)
        self.assertEqual(result["archiveDigest"], sha(payload))
        self.assertEqual(result["imageDigest"], digest)
        self.assertEqual(result["unpackedLayerBytes"], decoded)
        self.assertEqual(result["status"], "integrity_verified")

    def test_malformed_or_incomplete_evidence_is_refused(self):
        for fault in ("platform", "variant", "index-variant", "schema-type", "non-json", "diff-id", "descriptor-size", "external", "embedded", "missing", "multiple", "corrupt", "duplicate-json", "traversal", "duplicate-entry", "symlink"):
            with self.subTest(fault=fault):
                with self.assertRaises(ValueError):
                    self.verify(self.fixture(fault)[0])

    def test_archive_and_decompression_limits_and_candidate_binding(self):
        payload, _, decoded = self.fixture()
        for params in ({"max_bytes": len(payload) - 1}, {"max_unpacked_bytes": decoded - 1}, {"expected_digest": sha(b"different candidate")}):
            with self.subTest(params=params):
                with self.assertRaises(ValueError):
                    self.verify(payload, **params)

    def test_oversized_extension_is_rejected_before_tarfile_reads_it(self):
        header = tarfile.TarInfo("extended")
        header.type, header.size = tarfile.XHDTYPE, 1024**3
        with self.assertRaisesRegex(ValueError, "unsupported tar extension"):
            self.verify(header.tobuf(format=tarfile.USTAR_FORMAT) + bytes(1024))

    def test_truncation_and_appended_payload_are_refused(self):
        payload, _, _ = self.fixture()
        for changed in (payload[:500], payload + b"unexpected payload"):
            with self.assertRaises(ValueError):
                self.verify(changed)

    def test_cli_preserves_failure_and_emits_one_result(self):
        script = str(Path(__file__).with_name("verify-oci.py"))
        payload, digest, _ = self.fixture()
        with tempfile.TemporaryDirectory() as directory:
            archive = Path(directory) / "image.tar"
            archive.write_bytes(payload)
            base = [sys.executable, script, "--archive=" + str(archive), "--platform=linux/arm64"]
            for command, expected in ((base, 0), (base + ["--expected-digest=" + sha(b"wrong")], 5), (base + ["--expected-digest=invalid"], 2), (base + ["--max-bytes=0"], 2)):
                with self.subTest(expected=expected):
                    process = subprocess.run(command, capture_output=True, text=True)
                    result = json.loads(process.stdout)
                    self.assertEqual(process.returncode, expected)
                    self.assertEqual(result["ok"], expected == 0)
                    self.assertEqual(result["capability"], "oci.verify")
                    self.assertFalse(result["changed"])
                    if expected == 0:
                        self.assertEqual(result["result"]["imageDigest"], digest)
                    else:
                        self.assertEqual(result["error"]["code"], expected)

    @unittest.skipUnless(hasattr(os, "mkfifo"), "POSIX archive handling")
    def test_fifo_refused_without_waiting_for_writer(self):
        script = str(Path(__file__).with_name("verify-oci.py"))
        with tempfile.TemporaryDirectory() as directory:
            archive = Path(directory) / "fifo"
            os.mkfifo(archive)
            process = subprocess.run([sys.executable, script, "--archive=" + str(archive), "--platform=linux/arm64"], capture_output=True, text=True, timeout=5)
            self.assertEqual(process.returncode, 5)
            self.assertFalse(json.loads(process.stdout)["ok"])


if __name__ == "__main__":
    unittest.main()
