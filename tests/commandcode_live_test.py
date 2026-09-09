import base64
import copy
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
from urllib.parse import quote

from commandcode_live import check_secret, native, native_passed, safe_summary


class LiveSafetyTests(unittest.TestCase):
    def test_unreviewed_archive_never_launches_host(self):
        with tempfile.TemporaryDirectory() as directory:
            archive = Path(directory) / "unreviewed.zip"
            archive.write_bytes(b"not reviewed")
            with patch("commandcode_live.subprocess.Popen") as launch:
                with self.assertRaises(RuntimeError):
                    native("fixture-key", Path("host"), archive, "0" * 64)
                launch.assert_not_called()

    def test_secret_variants_rejected_without_echo(self):
        secret = "fixture+/credential"
        for value in (secret.encode(), quote(secret, safe="").encode(), base64.b64encode(secret.encode())):
            with self.assertRaises(RuntimeError) as caught:
                check_secret(secret, b"prefix" + value + b"suffix")
            self.assertNotIn(secret, str(caught.exception))
        check_secret(secret, b"unrelated")

    def test_summary_omits_content_identifiers_and_error_messages(self):
        result = safe_summary(403, [("Retry-After", "12"), ("X-Quota-Account", "private-account"), ("Authorization", "secret")], {
            "id": "private-id", "choices": [{"text": "private-content"}],
            "usage": {"total_tokens": 7, "account": "private-account"},
            "error": {"code": "upgrade_required", "message": "private-message"},
        })
        encoded = json.dumps(result)
        for value in ("private-account", "private-content", "private-id", "private-message", "secret"):
            self.assertNotIn(value, encoded)
        self.assertEqual(result["quota_headers"]["retry-after"], "12")
        self.assertEqual(result["usage"], {"total_tokens": 7})

    def test_failure_is_not_reported_as_qualification(self):
        result = {
            "secret_scan": "clean", "auth_files_created": 0, "shutdown_exit": 0,
            "frontend_error": {"status": 401},
            "nonstream": {"status": 200, "content_received": True, "usage": {"total_tokens": 3}},
            "stream": {"status": 200, "content_received": True, "usage": {"total_tokens": 4}, "terminal_received": True},
            "cancel": {"status": 200, "client_canceled": True},
        }
        self.assertTrue(native_passed(result))
        for mode, field, value in (("nonstream", "status", 503), ("stream", "usage", {}),
                                   ("stream", "terminal_received", False), ("cancel", "client_canceled", False)):
            bad = copy.deepcopy(result)
            bad[mode][field] = value
            self.assertFalse(native_passed(bad))
        result["auth_files_created"] = 1
        self.assertFalse(native_passed(result))
