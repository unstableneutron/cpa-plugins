#!/usr/bin/env python3
"""Canonical Linux package, extracted-library smoke, and explicit publication."""

import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shlex
import subprocess
import sys
import tempfile
import zipfile

REPO = "unstableneutron/cpa-plugins"
PLUGINS = ("commandcode", "chatgpt-backend", "embeddings-forward")
ARCHES = ("amd64", "arm64")


def run(*args, **kwargs):
    return subprocess.check_output(args, text=True, **kwargs).strip()


def require(condition, message):
    if not condition:
        raise ValueError(message)


def source(expected=None):
    commit = run("git", "rev-parse", "HEAD")
    require(not run("git", "status", "--porcelain"), "plugin source must be clean")
    require(not expected or commit == expected, "plugin source commit changed or differs from approved commit")
    return commit


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def archives(version, arch):
    return [f"{plugin}_{version}_linux_{arch}.zip" for plugin in PLUGINS]


def manifest(directory, names):
    return "".join(f"{digest(directory / name)}  {name}\n" for name in sorted(names))


def verify(directory, names):
    require((directory / "checksums.txt").read_text() == manifest(directory, names), "artifact checksum/set mismatch")


def write_info(directory, info, names):
    (directory / "BUILDINFO.json").write_text(json.dumps(info, indent=2, sort_keys=True) + "\n")
    (directory / "checksums.txt").write_text(manifest(directory, [*names, "BUILDINFO.json"]))


def host_info(path, arch, purego):
    # Whitelist build settings; never publish paths, arbitrary flags or environment.
    metadata = run("go", "version", "-m", str(path))
    settings = dict(re.findall(r"^\s*build\s+(\S+?)=(.*)$", metadata, re.M))
    tags = settings.get("-tags", "").strip('"').split(",")
    require(settings.get("GOOS") == "linux" and settings.get("GOARCH") == arch, "host target mismatch")
    require(settings.get("CGO_ENABLED") == ("0" if purego else "1"), "host CGO mode mismatch")
    require(("plugin_purego" in tags) == purego, "host loader tag mismatch")
    revision = settings.get("vcs.revision", "")
    require(re.fullmatch(r"[0-9a-f]{40}", revision) and settings.get("vcs.modified") == "false", "host must have clean source provenance")
    return {"sha256": digest(path), "commit": revision, "loader": "purego" if purego else "cgo", "target": f"linux/{arch}"}


def elf_info(path, arch):
    header = run("readelf", "-h", str(path))
    machine = "AArch64" if arch == "arm64" else "Advanced Micro Devices X86-64"
    require(re.search(r"Machine:\s+" + re.escape(machine) + r"\s*$", header, re.M), "ELF architecture mismatch")
    versions = run("readelf", "--version-info", str(path))
    dynamic = run("readelf", "-d", str(path))
    return {
        "machine": machine,
        "glibc_symbols": sorted(set(re.findall(r"GLIBC_[0-9.]+", versions))),
        "needed": re.findall(r"\(NEEDED\).*?\[(.*?)\]", dynamic),
    }


def package(version, arch, directory, commit):
    directory.mkdir(parents=True, exist_ok=True)
    # A failed rebuild must not leave an old qualification claim.
    (directory / "BUILDINFO.json").unlink(missing_ok=True)
    print(run("go", "run", "./cmd/package", "-version", version, "-goos", "linux", "-goarch", arch,
              "-out", str(directory), *PLUGINS))
    source(commit)
    compiler = shlex.split(os.environ.get("CC", "cc"))
    info = {"repository": REPO, "commit": commit, "version": version, "target": f"linux/{arch}",
            "abi": 1, "schemas": {"commandcode": 7, "chatgpt-backend-minimum-host": 8, "embeddings-forward-minimum-host": 9},
            "go": run("go", "env", "GOVERSION"), "gcc": run(*compiler, "-dumpfullversion")}
    write_info(directory, info, archives(version, arch))


def smoke(version, arch, directory, commit, cgo, purego):
    names = archives(version, arch)
    verify(directory, [*names, "BUILDINFO.json"])
    packaged = manifest(directory, names)
    info = json.loads((directory / "BUILDINFO.json").read_text())
    require((info["commit"], info["version"], info["target"]) == (commit, version, f"linux/{arch}"), "package provenance mismatch")
    info.pop("smoke", None)
    write_info(directory, info, names)
    hosts = [Path(cgo).resolve(), Path(purego).resolve()]
    provenance = [host_info(host, arch, mode) for host, mode in zip(hosts, (False, True))]
    require(provenance[0]["commit"] == provenance[1]["commit"], "smoke hosts must use the same source")
    native = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    require(platform.system() == "Linux", "Linux qualification requires Linux")
    runner = []
    execution = {"mode": "native", "glibc_runtime": run("getconf", "GNU_LIBC_VERSION")}
    if arch != native:
        require(native == "amd64" and arch == "arm64", "unsupported cross-execution target")
        sysroot = Path(os.environ.get("ARM64_SYSROOT", "/usr/aarch64-linux-gnu"))
        runner = ["qemu-aarch64", "-L", str(sysroot)]
        execution = {"mode": "qemu-user", "qemu": run("qemu-aarch64", "--version").splitlines()[0],
                     "libc_sha256": digest(sysroot / "lib/libc.so.6")}
    with tempfile.TemporaryDirectory(prefix="cpa-plugin-smoke-") as temporary:
        libs = Path(temporary)
        libraries = {}
        for plugin, name in zip(PLUGINS, names):
            with zipfile.ZipFile(directory / name) as archive:
                library = libs / f"{plugin}.so"
                library.write_bytes(archive.read(library.name))
            libraries[plugin] = elf_info(library, arch)
        for host in hosts:
            for environment in (False, True):
                for denial in ("unsupported_model", "upgrade_required"):
                    command = [sys.executable, "-B", "tests/native_e2e.py", "--arch", arch,
                               "--host", str(host), "--plugins", str(libs), "--qualification-failure", denial]
                    if environment:
                        command.append("--environment-auth")
                    if runner:
                        command += ["--runner", *runner]
                    subprocess.run(command, check=True)
            command = [sys.executable, "-B", "tests/forward_e2e.py", "--host", str(host), "--plugins", str(libs)]
            subprocess.run(command + (["--runner", *runner] if runner else []), check=True)
    source(commit)
    # Detect binaries or ZIPs replaced while qualification was running.
    require(provenance == [host_info(host, arch, mode) for host, mode in zip(hosts, (False, True))], "smoke hosts changed")
    verify(directory, [*names, "BUILDINFO.json"])
    require(manifest(directory, names) == packaged, "packages changed during smoke")
    info["smoke"] = {"hosts": provenance, "execution": execution, "libraries": libraries,
                     "accounts": ["file", "environment"], "denials": ["unsupported_model", "upgrade_required"]}
    write_info(directory, info, names)


def remote_source(commit):
    require(run("git", "branch", "--show-current") == "main", "publication requires main")
    require(run("gh", "api", f"repos/{REPO}/git/ref/heads/main", "--jq", ".object.sha") == commit,
            "publish source must equal repository main")


def publish(version, root, commit):
    remote_source(commit)
    tags = json.loads(run("gh", "api", f"repos/{REPO}/git/matching-refs/tags/v{version}"))
    require(not any(tag["ref"] == f"refs/tags/v{version}" for tag in tags), "tag already exists; inspect partial publication, never overwrite")
    for task in ("check", "test", "race", "vuln"):
        subprocess.run(["mise", "run", task], check=True)
    host_root = Path(os.environ["HOST_DIR"]).resolve()
    qualified = {}
    for arch in ARCHES:
        smoke(version, arch, root / f"linux_{arch}", commit,
              host_root / f"linux_{arch}/cpa-cgo", host_root / f"linux_{arch}/cpa-purego")
        qualified[arch] = manifest(root / f"linux_{arch}", [*archives(version, arch), "BUILDINFO.json"])
    source(commit)
    remote_source(commit)
    with tempfile.TemporaryDirectory(prefix="cpa-plugin-release-") as temporary:
        stage = Path(temporary)
        names = []
        for arch in ARCHES:
            directory = root / f"linux_{arch}"
            verify(directory, [*archives(version, arch), "BUILDINFO.json"])
            require(manifest(directory, [*archives(version, arch), "BUILDINFO.json"]) == qualified[arch], "qualified artifacts changed")
            for name in archives(version, arch):
                (stage / name).write_bytes((directory / name).read_bytes())
                names.append(name)
            name = f"BUILDINFO_linux_{arch}.json"
            (stage / name).write_bytes((directory / "BUILDINFO.json").read_bytes())
            names.append(name)
        (stage / "checksums.txt").write_text(manifest(stage, names))
        # Atomic create, not update: an existing/concurrently created tag fails closed.
        run("gh", "api", "--method", "POST", f"repos/{REPO}/git/refs",
            "-f", f"ref=refs/tags/v{version}", "-f", f"sha={commit}")
        print(run("gh", "release", "create", f"v{version}", "--repo", REPO, "--verify-tag",
                  "--title", f"Native plugins v{version}", "--prerelease", "--latest=false",
                  "--notes", "Command Code, ChatGPT backend and embeddings-forward; Linux amd64 and arm64. ABI 1; backend needs host schema 8, "
                  "embeddings-forward host schema 9. "
                  "See per-target BUILDINFO for libc requirements and native/emulated qualification. No live provider or macOS qualification.",
                  *(str(stage / name) for name in [*names, "checksums.txt"])))
        download = stage / "download"
        download.mkdir()
        run("gh", "release", "download", f"v{version}", "--repo", REPO, "--dir", str(download))
        require({p.name for p in download.iterdir()} == set(names + ["checksums.txt"]), "downloaded asset set mismatch")
        require((download / "checksums.txt").read_bytes() == (stage / "checksums.txt").read_bytes(), "downloaded manifest mismatch")
        verify(download, names)
        print((stage / "checksums.txt").read_text())


def main():
    version = os.environ.get("VERSION", "")
    require(re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?", version), "set VERSION without v")
    arch = os.environ.get("ARCH", "amd64")
    require(arch in ARCHES, "ARCH must be amd64 or arm64")
    commit = source(os.environ.get("PLUGIN_COMMIT"))
    root = Path("dist") / version
    directory = root / f"linux_{arch}"
    if sys.argv[1] == "package":
        package(version, arch, directory, commit)
    elif sys.argv[1] == "smoke":
        smoke(version, arch, directory, commit, os.environ["HOST_CGO"], os.environ["HOST_PUREGO"])
    elif sys.argv[1] == "publish":
        publish(version, root, commit)
    else:
        raise ValueError("unknown release operation")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError) as error:
        sys.exit(str(error))
