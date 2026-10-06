#!/usr/bin/env python3
"""Verify a bounded, self-contained OCI image archive without extracting it.

This is the release builder's narrow image profile: one Linux amd64/arm64 image,
SHA-256 blobs, and plain/gzip layers. Unsupported formats fail explicitly. This
proves content integrity and platform, not source provenance, vulnerability
clearance, publication authority, or deployment success.
"""
import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys
import tarfile
import tempfile
import zlib

INDEX = "application/vnd.oci.image.index.v1+json"
MANIFEST = "application/vnd.oci.image.manifest.v1+json"
CONFIG = "application/vnd.oci.image.config.v1+json"
LAYER = "application/vnd.oci.image.layer.v1.tar"
DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")
MAX_JSON = 4 * 1024 * 1024
MAX_ENTRIES = 8192
CHUNK = 1024 * 1024


def require(condition, reason):
    if not condition:
        raise ValueError(reason)


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "duplicate JSON field")
        result[key] = value
    return result


def invalid_constant(value):
    raise ValueError("non-JSON numeric constant")


def open_archive(path):
    # Opening a FIFO normally waits for a writer before fstat can reject it.
    # Nonblocking open lets the regular-file check apply to that case too.
    descriptor = os.open(path, os.O_RDONLY | os.O_NONBLOCK)
    try:
        require(stat.S_ISREG(os.fstat(descriptor).st_mode), "archive is not a regular file")
        return os.fdopen(descriptor, "rb")
    except BaseException:
        os.close(descriptor)
        raise


def digest_stream(stream, limit, sink=None):
    sha, size = hashlib.sha256(), 0
    while block := stream.read(min(CHUNK, limit - size + 1)):
        size += len(block)
        require(size <= limit, "content exceeds the byte limit")
        sha.update(block)
        if sink is not None:
            sink.write(block)
    return "sha256:" + sha.hexdigest(), size


def check_tar_headers(raw, archive_size):
    # tarfile reads PAX/GNU extension payloads before yielding a member. Refuse
    # them before that parser can allocate from an attacker-controlled length.
    # BuildKit's short SHA-256 names fit the ordinary USTAR transport profile.
    offset, entries = 0, 0
    while offset < archive_size:
        raw.seek(offset)
        header = raw.read(512)
        require(len(header) == 512, "truncated tar header")
        if header == bytes(512):
            require(archive_size - offset >= 1024, "missing tar end marker")
            while padding := raw.read(CHUNK):
                require(not any(padding), "nonzero data after tar end marker")
            return
        entries += 1
        require(entries <= MAX_ENTRIES, "too many archive entries")
        require(header[156:157] in (b"0", b"\0", b"5"), "unsupported tar extension or entry type")
        field = header[124:136].strip(b"\0 ")
        require(not field or re.fullmatch(b"[0-7]+", field), "unsupported tar size encoding")
        size = int(field or b"0", 8)
        offset += 512 + ((size + 511) // 512) * 512
        require(offset <= archive_size, "truncated tar entry")
    raise ValueError("missing tar end marker")


def verify(archive, platform, max_bytes, max_unpacked_bytes, expected_digest=None):
    require(platform in ("linux/amd64", "linux/arm64"), "unsupported expected platform")
    require(max_bytes > 0 and max_unpacked_bytes > 0, "byte limits must be positive")
    if expected_digest is not None:
        require(DIGEST.fullmatch(expected_digest), "expected digest must be SHA-256")
    # No directory materialization, symlink following inside the archive, or
    # remote descriptor fetch. Every digest must be present and verified here.
    with open_archive(archive) as source, tempfile.TemporaryFile() as raw:
        original = os.fstat(source.fileno())
        require(stat.S_ISREG(original.st_mode) and 0 < original.st_size <= max_bytes, "archive is not a bounded regular file")
        # Snapshot into an anonymous bounded file. Metadata and layers cannot
        # come from different revisions of the caller's mutable archive path.
        archive_digest, archive_bytes = digest_stream(source, max_bytes, raw)
        raw.flush()
        before = os.fstat(raw.fileno())
        check_tar_headers(raw, archive_bytes)
        raw.seek(0)
        with tarfile.open(fileobj=raw, mode="r:") as tar:
            members = {}
            for member in tar:
                require(len(members) < MAX_ENTRIES, "too many archive entries")
                require(member.name not in members, "duplicate archive entry")
                name = member.name
                if member.isdir():
                    require(name.rstrip("/") in ("blobs", "blobs/sha256"), "unsupported archive directory")
                else:
                    require(member.isreg() and not member.sparse, "archive links, sparse files and special entries are refused")
                    require(name in ("oci-layout", "index.json") or re.fullmatch(r"blobs/sha256/[0-9a-f]{64}", name), "unsupported archive path")
                    require(0 <= member.size <= max_bytes, "archive entry exceeds the byte limit")
                    with tar.extractfile(member) as stream:
                        digest, size = digest_stream(stream, max_bytes)
                    require(size == member.size, "truncated archive entry")
                    if name.startswith("blobs/"):
                        require(digest == "sha256:" + name.rsplit("/", 1)[1], "blob content does not match its SHA-256 name")
                members[name] = member

            def read_json(name):
                require(name in members and members[name].isreg(), "required JSON object is absent")
                require(members[name].size <= MAX_JSON, "JSON object exceeds the byte limit")
                with tar.extractfile(members[name]) as source:
                    result = json.load(source, object_pairs_hook=unique_object, parse_constant=invalid_constant)
                require(isinstance(result, dict), "JSON metadata must be an object")
                return result

            def descriptor(value, media_type):
                require(isinstance(value, dict), "descriptor must be an object")
                require(value.get("mediaType") == media_type, "unsupported descriptor media type")
                require(not value.get("urls") and "data" not in value, "external and embedded descriptors are refused")
                digest = value.get("digest", "")
                require(isinstance(digest, str) and DIGEST.fullmatch(digest), "descriptor must use SHA-256")
                name = "blobs/sha256/" + digest.split(":", 1)[1]
                require(name in members and members[name].isreg(), "referenced blob is absent")
                require(type(value.get("size")) is int and value["size"] == members[name].size, "descriptor size does not match its blob")
                return name

            require(read_json("oci-layout").get("imageLayoutVersion") == "1.0.0", "unsupported OCI layout version")
            index = read_json("index.json")
            require(type(index.get("schemaVersion")) is int and index["schemaVersion"] == 2 and index.get("mediaType", INDEX) == INDEX, "invalid OCI index")
            manifests = index.get("manifests")
            require(isinstance(manifests, list) and len(manifests) == 1, "expected exactly one image manifest")
            image = manifests[0]
            manifest = read_json(descriptor(image, MANIFEST))
            require(type(manifest.get("schemaVersion")) is int and manifest["schemaVersion"] == 2 and manifest.get("mediaType", MANIFEST) == MANIFEST, "invalid image manifest")
            require("subject" not in manifest and "artifactType" not in manifest, "auxiliary artifacts are outside the image profile")
            if expected_digest is not None:
                require(image["digest"] == expected_digest, "image digest differs from the expected candidate")
            config = read_json(descriptor(manifest.get("config"), CONFIG))
            system, arch = platform.split("/")
            require(config.get("os") == system and config.get("architecture") == arch, "image config differs from the expected platform")
            declared = image.get("platform", {})
            require(isinstance(declared, dict) and (not declared or (declared.get("os") == system and declared.get("architecture") == arch)), "index and image platform disagree")
            # The caller selects an architecture, not a more specific CPU ABI.
            # Accept only its baseline variant; otherwise that weaker selection
            # could approve an image the target CPU cannot execute.
            variants = (None, "", "v8") if arch == "arm64" else (None, "")
            for value in (config, declared):
                require(value.get("variant") in variants and not value.get("os.features") and not value.get("os.version"), "specialized platform requirements are unsupported")
            layers, rootfs = manifest.get("layers"), config.get("rootfs")
            require(isinstance(rootfs, dict) and rootfs.get("type") == "layers", "image has no layer rootfs")
            diffs = rootfs.get("diff_ids")
            require(isinstance(layers, list) and isinstance(diffs, list) and len(layers) == len(diffs), "rootfs and manifest layers disagree")
            unpacked = 0
            for layer, expected_diff in zip(layers, diffs):
                require(isinstance(layer, dict) and layer.get("mediaType") in (LAYER, LAYER + "+gzip"), "unsupported layer compression")
                name = descriptor(layer, layer["mediaType"])
                require(isinstance(expected_diff, str) and DIGEST.fullmatch(expected_diff), "rootfs DiffID must use SHA-256")
                with tar.extractfile(members[name]) as source:
                    if layer["mediaType"].endswith("+gzip"):
                        with gzip.GzipFile(fileobj=source) as decoded:
                            actual, size = digest_stream(decoded, max_unpacked_bytes - unpacked)
                    else:
                        actual, size = digest_stream(source, max_unpacked_bytes - unpacked)
                unpacked += size
                require(actual == expected_diff, "uncompressed layer differs from the image rootfs DiffID")
        # Archive contents must still describe the bytes whose digest is returned.
        # The publisher must consume that same immutable object by this digest.
        raw.seek(0)
        after_digest, after_size = digest_stream(raw, max_bytes)
        after = os.fstat(raw.fileno())
        require((archive_digest, archive_bytes, before.st_mtime_ns) == (after_digest, after_size, after.st_mtime_ns), "archive changed during verification")
    return {"status": "integrity_verified", "archiveDigest": archive_digest, "archiveBytes": archive_bytes,
            "imageDigest": image["digest"], "configDigest": manifest["config"]["digest"],
            "platform": platform, "layers": len(layers), "unpackedLayerBytes": unpacked}


def positive(value):
    number = int(value)
    if number < 1:
        raise argparse.ArgumentTypeError("must be positive")
    return number


class ParametersInvalid(ValueError):
    pass


class Parser(argparse.ArgumentParser):
    def error(self, message):
        raise ParametersInvalid(message)


def emit(result=None, error=None, code=0):
    print(json.dumps({"ok": code == 0, "capability": "oci.verify", "changed": False,
                      "result": result or {}, "error": {"code": code, "message": error} if code else None}))
    return code


def main():
    parser = Parser(description=__doc__)
    parser.add_argument("--archive", type=Path, required=True)
    parser.add_argument("--platform", choices=("linux/amd64", "linux/arm64"), required=True)
    parser.add_argument("--expected-digest")
    parser.add_argument("--max-bytes", type=positive, default=2 * 1024**3)
    parser.add_argument("--max-unpacked-bytes", type=positive, default=8 * 1024**3)
    try:
        args = parser.parse_args()
        if args.expected_digest is not None and not DIGEST.fullmatch(args.expected_digest):
            raise ParametersInvalid("expected digest must be SHA-256")
    except ParametersInvalid as exc:
        return emit(error=str(exc), code=2)
    try:
        result = verify(args.archive, args.platform, args.max_bytes, args.max_unpacked_bytes, args.expected_digest)
    except (OSError, ValueError, EOFError, RecursionError, tarfile.TarError, zlib.error) as exc:
        return emit(error=str(exc), code=5)
    return emit(result=result)


if __name__ == "__main__":
    sys.exit(main())
