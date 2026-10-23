#!/usr/bin/env python3
"""Mechanical disposable-provider fixture for ci.delivery-artifacts.

No installation credentials, release decisions, or active database inputs.
The caller chooses immutable images; this fixture only runs fixed tests.
"""

import argparse
import base64
import hashlib
import http.client
import io
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import subprocess
import sys
import tarfile
import time
import urllib.error
import urllib.request
import uuid


LABEL = "io.memql.delivery-qualification"
PIN = re.compile(r"^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$")
MAX_ARCHIVE = 2 << 30
MAX_JSON = 4 << 20
DATABASE_BOOT = """function main() {
    printf '%s\\n' "$POSTGRES_PASSWORD" > /fixture/password
    initdb -D /fixture/data --encoding=UTF8 --locale=C --username=memql \\
      --pwfile=/fixture/password --auth-local=trust --auth-host=scram-sha-256
    rm /fixture/password
    printf '%s\\n' 'host all all 0.0.0.0/0 scram-sha-256' >> /fixture/data/pg_hba.conf
    exec postgres -D /fixture/data -c listen_addresses=* \\
      -c shared_preload_libraries=timescaledb -c fsync=off \\
      -c synchronous_commit=off -c full_page_writes=off \\
      -c max_wal_size=128MB -c checkpoint_timeout=1min
}; main"""
TESTS = [
    ("./integrations/azureblob", "^TestAzurite", [
        "TestAzuriteVerifiedArchiveRehearsal", "TestAzuriteVerifiedStreamLargeObjectAndRecovery",
        "TestAzuriteVerifiedEmptyStream", "TestAzuriteRetirementStopsAnAlreadyAdmittedUpload",
        "TestAzuriteRetirementReconcilesLostResponsesAndLeaseGap",
        "TestAzuriteRetirementRefusesChangedContentAndETag",
        "TestAzuriteRetirementFencesLateWritersAndClearsStaging",
        "TestAzuriteReceiptStreamExactVersion", "TestAzuriteStagedBlockLifecycle",
        "TestAzuriteInventoryOfAnUnknownObjectIsEmpty", "TestAzuriteStoredBlobURLsRoundTripNestedEscapedPaths"]),
    ("./integrations/ociregistry", "^TestDistributionRegistry$", ["TestDistributionRegistry"]),
    ("./integrations/release", "^TestCandidatePublicationWithDisposableDistribution$", [
        "TestCandidatePublicationWithDisposableDistribution"]),
    ("./app", "^(TestReleaseCandidateThroughNativeLibraryAndSecondEngine|TestPipelineStreamAzuriteDBRecoveryAcrossClients|TestPipelineLifecycleAzuriteDBRetirementRecoveryAcrossClients)$", [
        "TestReleaseCandidateThroughNativeLibraryAndSecondEngine",
        "TestPipelineStreamAzuriteDBRecoveryAcrossClients",
        "TestPipelineLifecycleAzuriteDBRetirementRecoveryAcrossClients"]),
]


def digest_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b""):
            h.update(chunk)
    return "sha256:" + h.hexdigest()


def export_platform(source, target):
    """Project Docker's OCI export to one exact platform, never rebuild bytes."""
    with tarfile.open(source, "r:") as archive:
        members = {}
        for member in archive:
            if len(members) >= 8192:
                raise ValueError("Docker export exceeds the OCI entry bound")
            name = member.name.removeprefix("./")
            if name.startswith("/") or ".." in name.split("/") or name in members:
                raise ValueError("unsafe or duplicate Docker export entry")
            if not (member.isdir() or member.isfile()) or member.size > MAX_ARCHIVE:
                raise ValueError("Docker export must contain bounded regular files")
            members[name] = member

        def read_json(name):
            member = members[name]
            if member.size > MAX_JSON:
                raise ValueError("oversized OCI metadata")
            return json.load(archive.extractfile(member))

        def blob_name(descriptor):
            digest = descriptor.get("digest", "")
            if not re.fullmatch(r"sha256:[a-f0-9]{64}", digest):
                raise ValueError("invalid OCI digest")
            name = "blobs/sha256/" + digest[7:]
            if name not in members:
                raise ValueError("missing OCI blob for a selected or unclassified descriptor: " + digest)
            member = members[name]
            if member.size != descriptor.get("size"):
                raise ValueError("OCI descriptor size mismatch")
            h = hashlib.sha256()
            with archive.extractfile(member) as stream:
                for chunk in iter(lambda: stream.read(1 << 20), b""):
                    h.update(chunk)
            if h.hexdigest() != digest[7:]:
                raise ValueError("OCI descriptor digest mismatch")
            return name

        candidates = []

        def visit(descriptor, depth=0):
            if depth > 4:
                raise ValueError("OCI index nesting exceeds fixture bound")
            platform = descriptor.get("platform")
            if platform is not None:
                if not isinstance(platform, dict) or not all(isinstance(platform.get(k), str) and platform[k] for k in ("os", "architecture")):
                    raise ValueError("invalid OCI platform descriptor")
                if (platform["os"], platform["architecture"]) != ("linux", "arm64"):
                    # Docker's containerd store can export an index whose other
                    # platforms were never pulled. Their blobs are unnecessary
                    # for this projection; selected/unclassified descriptors
                    # still require complete, digest-verified contents below.
                    return
            obj = read_json(blob_name(descriptor))
            if "manifests" in obj:
                for child in obj["manifests"]:
                    visit(child, depth + 1)
            elif "config" in obj and "layers" in obj:
                config = read_json(blob_name(obj["config"]))
                if (config.get("os"), config.get("architecture")) == ("linux", "arm64"):
                    candidates.append((descriptor, obj))
                elif platform is not None:
                    raise ValueError("selected OCI platform disagrees with image config")

        for descriptor in read_json("index.json")["manifests"]:
            visit(descriptor)
        if len(candidates) != 1:
            raise ValueError("artifact must contain exactly one linux/arm64 image")
        descriptor, manifest = candidates[0]
        descriptors = [descriptor, manifest["config"], *manifest["layers"]]
        paths = sorted(set(blob_name(item) for item in descriptors))
        if sum(members[path].size for path in paths) > MAX_ARCHIVE:
            raise ValueError("OCI image exceeds 2 GiB fixture bound")
        clean = {key: descriptor[key] for key in ("mediaType", "size", "digest")}
        clean["platform"] = {"os": "linux", "architecture": "arm64"}
        with tarfile.open(target, "w", format=tarfile.USTAR_FORMAT) as out:
            for name, value in (("oci-layout", {"imageLayoutVersion": "1.0.0"}),
                                ("index.json", {"schemaVersion": 2, "manifests": [clean]})):
                body = json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
                member = tarfile.TarInfo(name)
                member.size, member.mode = len(body), 0o644
                out.addfile(member, io.BytesIO(body))
            for path in paths:
                member = tarfile.TarInfo(path)
                member.size, member.mode = members[path].size, 0o644
                out.addfile(member, archive.extractfile(members[path]))
        return descriptor["digest"]


def verify_events(raw, required):
    events = [json.loads(line) for line in raw.splitlines() if line.startswith("{")]
    skipped = [e.get("Test", e.get("Package")) for e in events if e.get("Action") == "skip"]
    passed = {e["Test"] for e in events if e.get("Action") == "pass" and "Test" in e}
    missing = sorted(set(required) - passed)
    if skipped or missing:
        raise ValueError(f"qualification skipped tests {skipped} or lacked passing evidence {missing}")
    return sorted(passed)


class Fixture:
    def __init__(self, root, output):
        self.root = root
        self.token = uuid.uuid4().hex
        self.output = Path(output).resolve() / self.token
        self.output.mkdir(parents=True, mode=0o700)
        self.containers = []
        self.environment = {k: v for k, v in os.environ.items() if not k.startswith("MEMQL_")}
        self.environment.update(GOMAXPROCS="2", GOFLAGS="-p=1")
        self.deadline = time.monotonic() + 1800
        self.report = {"state": "running", "evidence": str(self.output), "tests": [],
                       "scope": "disposable blob/OCI publication and durable receipts; not an installed rollout"}

    def run(self, argv, timeout=120, env=None, log=None, check=True):
        timeout = min(timeout, max(1, self.deadline - time.monotonic()))
        proc = subprocess.Popen(argv, cwd=self.root, env=env or self.environment,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True)
        try:
            raw, _ = proc.communicate(timeout=timeout)
        except BaseException:
            os.killpg(proc.pid, signal.SIGTERM)
            try:
                interrupted, _ = proc.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(proc.pid, signal.SIGKILL)
                interrupted, _ = proc.communicate()
            if log:
                (self.output / log).write_bytes(interrupted)
            raise
        text = raw.decode("utf-8", errors="replace")
        if log:
            (self.output / log).write_text(text)
        if check and proc.returncode:
            # Command arguments can contain ephemeral fixture credentials.
            raise RuntimeError(f"{argv[0]} failed ({proc.returncode}); {log or 'fixture setup'}")
        return proc.returncode, text

    def docker(self, *args, **kwargs):
        return self.run(["docker", *args], **kwargs)

    def image(self, ref, platform=None):
        # inspect is a cache/idempotency branch, never a trust substitution.
        code, raw = self.docker("image", "inspect", ref, check=False)
        present = code == 0
        if present and platform:
            cached = json.loads(raw)[0]
            present = cached.get("Os", "") + "/" + cached.get("Architecture", "") == platform
        if not present:
            print("Fetching pinned fixture image", file=sys.stderr)
            self.docker("pull", *(["--platform=" + platform] if platform else []), ref,
                        timeout=600, log="pull-" + str(len(list(self.output.glob('pull-*')))))

    def start(self, kind, image, port, args, command=()):
        name = "memql-qualification-" + self.token[:12] + "-" + kind
        self.containers.append(name)  # record before send: recover a lost creation reply
        self.docker("run", "-d", "--name", name, "--label", LABEL + "=" + self.token,
                    "--pull=never", "--cpus=2", "--memory=768m", "--pids-limit=256",
                    "-p", "127.0.0.1::" + str(port), *args, image, *command)
        _, raw = self.docker("inspect", name)
        bindings = json.loads(raw)[0]["NetworkSettings"]["Ports"][str(port) + "/tcp"]
        if len(bindings) != 1 or bindings[0]["HostIp"] != "127.0.0.1":
            raise RuntimeError("fixture port is not confined to loopback")
        return name, int(bindings[0]["HostPort"])

    def wait_http(self, port, path):
        for _ in range(60):
            try:
                with urllib.request.urlopen(f"http://127.0.0.1:{port}{path}", timeout=1):
                    return
            except urllib.error.HTTPError as exc:
                if exc.code in (400, 403):  # Azurite requires authenticated requests.
                    return
            except (urllib.error.URLError, TimeoutError, ConnectionError, http.client.HTTPException):
                pass
            time.sleep(1)
        raise RuntimeError("provider did not become ready within 60 seconds")

    def cleanup(self):
        failed = []
        # Cleanup gets its own bounded allowance after a test timeout.
        self.deadline = max(self.deadline, time.monotonic() + 90)
        for name in reversed(self.containers):
            try:
                code, raw = self.docker("inspect", name, check=False, timeout=10)
                if code:
                    # A failed inspect or unavailable daemon is not disappearance.
                    _, remaining = self.docker("ps", "-aq", "--filter", "name=^/" + name + "$", timeout=10)
                    if remaining.strip():
                        raise RuntimeError("fixture remains but its ownership cannot be read")
                    continue
                item = json.loads(raw)[0]
                if item["Config"]["Labels"].get(LABEL) != self.token:
                    raise RuntimeError("fixture ownership label changed")
                self.docker("logs", name, check=False, timeout=10, log=name + ".log")
                self.docker("rm", "-f", "-v", name, timeout=20)
                code, remaining = self.docker("ps", "-aq", "--filter", "name=^/" + name + "$", timeout=10)
                if code or remaining.strip():
                    raise RuntimeError("container removal unconfirmed")
            except Exception:
                failed.append(name)
        self.report["cleanup"] = {"confirmed": not failed, "remaining": failed}
        if failed:
            raise RuntimeError("owned fixture cleanup requires attention: " + ", ".join(failed))

    def qualify(self, args):
        _, commit = self.run(["git", "rev-parse", "HEAD"])
        commit = commit.strip()
        if not re.fullmatch("[a-f0-9]{40}", commit):
            raise ValueError("qualification requires an immutable source commit")
        _, changes = self.run(["git", "status", "--porcelain"])
        _, go_version = self.run(["go", "version"])
        _, docker_version = self.docker("version", "--format", "{{.Client.Version}} / {{.Server.Version}}")
        self.report.update(commit=commit, dirty=bool(changes.strip()), images={
            k: getattr(args, k) for k in ("database_image", "blob_image", "registry_image", "artifact_image")})
        self.report["tools"] = {"go": go_version.strip(), "dockerClientAndServer": docker_version.strip()}
        for ref in (args.database_image, args.blob_image, args.registry_image):
            self.image(ref)
        self.image(args.artifact_image, "linux/arm64")
        _, image_info = self.docker("image", "inspect", args.artifact_image)
        if not 0 < json.loads(image_info)[0]["Size"] <= MAX_ARCHIVE:
            raise ValueError("artifact exceeds 2 GiB export bound")
        raw = self.output / "docker-export.tar"
        archive = self.output / "artifact.oci.tar"
        self.docker("image", "save", "-o", str(raw), args.artifact_image, timeout=180)
        if raw.stat().st_size > MAX_ARCHIVE:
            raise ValueError("Docker export exceeds 2 GiB fixture bound")
        image_digest = export_platform(raw, archive)
        raw.unlink()
        archive_digest = digest_file(archive)
        self.report["artifact"] = {"platform": "linux/arm64", "imageDigest": image_digest,
                                   "archiveDigest": archive_digest, "bytes": archive.stat().st_size}
        key = base64.b64encode(secrets.token_bytes(64)).decode()
        password = secrets.token_hex(24)
        database, db_port = self.start("db", args.database_image, 5432,
            ["--tmpfs", "/fixture:rw,size=768m,mode=1777", "--user", "postgres", "--entrypoint", "bash",
             "-e", "POSTGRES_PASSWORD=" + password], ["-ec", DATABASE_BOOT])
        _, blob_port = self.start("blob", args.blob_image, 10000,
            ["--tmpfs", "/data:rw,size=768m", "-e", "AZURITE_ACCOUNTS=deliveryfixture:" + key],
            ["azurite-blob", "--blobHost", "0.0.0.0", "--location", "/data", "--silent", "--skipApiVersionCheck"])
        _, registry_port = self.start("registry", args.registry_image, 5000,
            ["--tmpfs", "/var/lib/registry:rw,size=768m"])
        for _ in range(60):
            code, _ = self.docker("exec", database, "pg_isready", "-h", "127.0.0.1", "-U", "memql", check=False)
            if code == 0:
                break
            time.sleep(1)
        else:
            raise RuntimeError("fixture database did not become ready within 60 seconds")
        self.docker("exec", database, "createdb", "-U", "memql", "delivery")
        self.docker("exec", database, "psql", "-U", "memql", "-d", "delivery", "-v", "ON_ERROR_STOP=1",
                    "-c", "CREATE EXTENSION IF NOT EXISTS timescaledb; CREATE EXTENSION IF NOT EXISTS vector;",
                    log="database-init.log")
        self.wait_http(blob_port, "/deliveryfixture?comp=list")
        self.wait_http(registry_port, "/v2/")
        env = dict(self.environment)
        env.update(MEMQL_REQUIRE_DB="1", MEMQL_DATABASE_DSN=f"postgres://memql:{password}@127.0.0.1:{db_port}/delivery?sslmode=disable",
                   MEMQL_AZURITE_TEST_CONNECTION_STRING=f"DefaultEndpointsProtocol=http;AccountName=deliveryfixture;AccountKey={key};BlobEndpoint=http://127.0.0.1:{blob_port}/deliveryfixture;",
                   MEMQL_OCI_TEST_REGISTRY=f"http://127.0.0.1:{registry_port}",
                   MEMQL_CANDIDATE_TEST_OCI_ARCHIVE=str(archive), MEMQL_CANDIDATE_TEST_ARCHIVE_DIGEST=archive_digest,
                   MEMQL_CANDIDATE_TEST_IMAGE_DIGEST=image_digest, MEMQL_VERIFIED_STREAM_REHEARSAL_FILE=str(archive))
        self.run(["bash", "scripts/identity/build-css.sh"], timeout=180, log="identity-css.log")
        for package, pattern, required in TESTS:
            print("Verifying " + package + " against disposable providers", file=sys.stderr)
            name = package.strip("./").replace("/", "-")
            code, raw = self.run(["go", "test", "-p", "1", "-count=1", "-timeout=8m", "-json",
                "-ldflags", "-X github.com/znasllc-io/memql/core/buildinfo.commit=" + commit,
                package, "-run", pattern], timeout=600, env=env, log=name + ".jsonl", check=False)
            human = "".join(e.get("Output", "") for e in (json.loads(line) for line in raw.splitlines() if line.startswith("{")))
            (self.output / (name + ".log")).write_text(human)
            if code:
                print(human[-6000:], file=sys.stderr)
                raise RuntimeError(package + " qualification failed; see retained log")
            passed = verify_events(raw, required)
            self.report["tests"].append({"package": package, "passed": passed})
            print(f"Passed {len(passed)} tests in {package}", file=sys.stderr)


def main():
    parser = argparse.ArgumentParser()
    for name in ("database-image", "blob-image", "registry-image", "artifact-image", "output"):
        parser.add_argument("--" + name, required=True)
    args = parser.parse_args()
    for name in ("database_image", "blob_image", "registry_image", "artifact_image"):
        if not PIN.fullmatch(getattr(args, name)):
            print("Fixture images must use explicit sha256 digest pins", file=sys.stderr)
            return 2
    for tool in ("docker", "go", "git", "bash"):
        if not shutil.which(tool):
            print(tool + " is required", file=sys.stderr)
            return 4
    try:
        fixture = Fixture(Path(__file__).resolve().parents[2], args.output)
    except OSError as exc:
        print("Cannot create qualification evidence directory: " + str(exc), file=sys.stderr)
        return 5
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(InterruptedError("qualification cancelled")))
    status = 0
    try:
        fixture.qualify(args)
        fixture.report["state"] = "passed"
    except (Exception, KeyboardInterrupt) as exc:
        fixture.report.update(state="failed", error=str(exc))
        print(str(exc), file=sys.stderr)
        status = 5
    finally:
        try:
            fixture.cleanup()
        except Exception as exc:
            fixture.report.update(state="failed", error=str(exc))
            status = 5
        report = fixture.output / "result.json"
        report.write_text(json.dumps(fixture.report, indent=2) + "\n")
        print(json.dumps(fixture.report, separators=(",", ":")))
    return status


if __name__ == "__main__":
    sys.exit(main())
