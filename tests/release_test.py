"""Exercise publication failure paths without invoking git, compilers or GitHub."""

import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
import zipfile

spec = importlib.util.spec_from_file_location("release", Path(__file__).parents[1] / "tools/release.py")
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)
COMMIT = "a" * 40
VERSION = "0.2.3"


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        for arch in release.ARCHES:
            directory = self.root / f"linux_{arch}"
            directory.mkdir()
            for plugin, name in zip(release.PLUGINS, release.archives(VERSION, arch)):
                with zipfile.ZipFile(directory / name, "w") as archive:
                    archive.writestr(f"{plugin}.so", f"{plugin}-{arch}")
            release.write_info(directory, {"commit": COMMIT, "version": VERSION, "target": f"linux/{arch}"}, release.archives(VERSION, arch))

    def test_source_pin_and_cleanliness(self):
        for expected, dirty in (("b" * 40, ""), (COMMIT, " M file")):
            with self.subTest(expected=expected, dirty=dirty), patch.object(release, "run", side_effect=[COMMIT, dirty]):
                with self.assertRaises(ValueError):
                    release.source(expected)
        with patch.object(release, "run", side_effect=[COMMIT, ""]):
            self.assertEqual(release.source(), COMMIT)

    def test_host_modes_targets_and_sanitized_provenance(self):
        host = self.root / "host"
        host.write_bytes(b"binary")
        metadata = "\n".join(f"\tbuild\t{k}={v}" for k, v in {
            "GOOS": "linux", "GOARCH": "arm64", "CGO_ENABLED": "0", "-tags": "plugin_purego",
            "vcs.revision": COMMIT, "vcs.modified": "false", "-ldflags": "secret-not-for-publication",
        }.items())
        with patch.object(release, "run", return_value=metadata):
            info = release.host_info(host, "arm64", True)
            self.assertNotIn("secret", json.dumps(info))
            self.assertNotIn(str(host), json.dumps(info))
            with self.assertRaisesRegex(ValueError, "CGO"):
                release.host_info(host, "arm64", False)
            with self.assertRaisesRegex(ValueError, "target"):
                release.host_info(host, "amd64", True)
        for old, new in (("modified=false", "modified=true"), ("plugin_purego", "other")):
            with patch.object(release, "run", return_value=metadata.replace(old, new)):
                with self.assertRaises(ValueError):
                    release.host_info(host, "arm64", True)

    def test_corrupt_package_fails_before_execution(self):
        directory = self.root / "linux_amd64"
        (directory / release.archives(VERSION, "amd64")[0]).write_bytes(b"corrupt")
        with patch.object(release.subprocess, "run") as execute:
            with self.assertRaisesRegex(ValueError, "checksum"):
                release.smoke(VERSION, "amd64", directory, COMMIT, "cgo", "purego")
            execute.assert_not_called()

    def test_smoke_consumes_archives_all_modes_and_invalidates_failure(self):
        directory = self.root / "linux_amd64"
        before = release.manifest(directory, release.archives(VERSION, "amd64"))
        with patch.object(release, "source"), patch.object(release, "host_info", return_value={"commit": COMMIT}), \
             patch.object(release, "elf_info", return_value={}), patch.object(release, "run", return_value="glibc 2.36"), \
             patch.object(release.platform, "machine", return_value="x86_64"), \
             patch.object(release.platform, "system", return_value="Linux"), \
             patch.object(release.subprocess, "run") as execute:
            release.smoke(VERSION, "amd64", directory, COMMIT, "cgo", "purego")
            commands = [call.args[0] for call in execute.call_args_list]
            self.assertEqual(len(commands), 8)
            self.assertEqual(sum("--environment-auth" in cmd for cmd in commands), 4)
            self.assertTrue(all("tests/native_e2e.py" in cmd for cmd in commands))
            self.assertEqual(release.manifest(directory, release.archives(VERSION, "amd64")), before)
            execute.side_effect = subprocess.CalledProcessError(1, "fixture")
            with self.assertRaises(subprocess.CalledProcessError):
                release.smoke(VERSION, "amd64", directory, COMMIT, "cgo", "purego")
            self.assertNotIn("smoke", json.loads((directory / "BUILDINFO.json").read_text()))

    def test_publication_failures_and_exact_download(self):
        for scenario in ("success", "tag", "origin", "source", "gate", "arm64", "create", "download", "corrupt", "extra"):
            with self.subTest(scenario=scenario):
                calls, uploaded = [], {}

                def command(*args):
                    calls.append(args)
                    if "matching-refs" in " ".join(args):
                        return json.dumps([{"ref": f"refs/tags/v{VERSION}"}]) if scenario == "tag" else "[]"
                    if args[:3] == ("gh", "release", "create"):
                        if scenario == "create":
                            raise subprocess.CalledProcessError(1, "gh create")
                        self.assertIn("--verify-tag", args)
                        self.assertIn("--prerelease", args)
                        self.assertIn("--latest=false", args)
                        for name in args:
                            if name.startswith("/"):
                                path = Path(name)
                                uploaded[path.name] = path.read_bytes()
                        self.assertEqual(len(uploaded), 7)
                    if args[:3] == ("gh", "release", "download"):
                        if scenario == "download":
                            raise subprocess.CalledProcessError(1, "gh download")
                        destination = Path(args[-1])
                        for name, data in uploaded.items():
                            (destination / name).write_bytes(data)
                        if scenario == "corrupt":
                            (destination / release.archives(VERSION, "arm64")[0]).write_bytes(b"wrong")
                        if scenario == "extra":
                            (destination / "unqualified.zip").write_bytes(b"extra")
                    return ""

                def smoke(*args):
                    if scenario == "arm64" and args[1] == "arm64":
                        raise ValueError("arm64 failed")

                with patch.dict(os.environ, {"HOST_DIR": str(self.root)}), \
                     patch.object(release, "run", side_effect=command), \
                     patch.object(release, "remote_source", side_effect=ValueError("origin moved") if scenario == "origin" else None), \
                     patch.object(release, "source", side_effect=ValueError("source moved") if scenario == "source" else None), \
                     patch.object(release.subprocess, "run", side_effect=ValueError("gate failed") if scenario == "gate" else None), \
                     patch.object(release, "smoke", side_effect=smoke):
                    if scenario == "success":
                        release.publish(VERSION, self.root, COMMIT)
                    else:
                        with self.assertRaises((ValueError, subprocess.CalledProcessError)):
                            release.publish(VERSION, self.root, COMMIT)
                    writes = [cmd for cmd in calls if "POST" in cmd]
                    self.assertEqual(len(writes), 0 if scenario in ("tag", "origin", "source", "gate", "arm64") else 1)
                    self.assertFalse(any("delete" in cmd or "--clobber" in cmd for cmd in calls))


if __name__ == "__main__":
    unittest.main()
