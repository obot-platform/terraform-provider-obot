"""Behavior checks for test tooling; no Docker or network is required."""
import argparse
import hashlib
import io
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from unittest.mock import patch

import obot


def bundle(name="obot", payload=b"test executable"):
    stream = io.BytesIO()
    with tarfile.open(fileobj=stream, mode="w:gz") as archive:
        member = tarfile.TarInfo(name)
        member.size = len(payload)
        archive.addfile(member, io.BytesIO(payload))
    return stream.getvalue()


class ToolingTest(unittest.TestCase):
    def setUp(self):
        environment = patch.dict(obot.os.environ)
        environment.start()
        self.addCleanup(environment.stop)
        obot.os.environ.pop("OBOT_SOURCE_DIR", None)


class ArtifactTests(ToolingTest):
    def test_release_checksum_failure_leaves_no_executable(self):
        with tempfile.TemporaryDirectory() as directory:
            asset = "obot_v0.26.3_linux_arm64.tar.gz"
            with patch.object(obot, "CACHE", Path(directory)), \
                    patch.object(obot.platform, "system", return_value="Linux"), \
                    patch.object(obot.platform, "machine", return_value="aarch64"), \
                    patch.object(obot, "download", side_effect=[
                        ("0" * 64 + "  " + asset).encode(), bundle()]):
                with self.assertRaisesRegex(obot.SetupError, "Checksum mismatch"):
                    obot.cli("v0.26.3")
                self.assertFalse(any(Path(directory).rglob("obot")))

    def test_release_download_is_verified_cached_and_executable(self):
        with tempfile.TemporaryDirectory() as directory:
            archive = bundle()
            asset = "obot_v0.26.3_linux_arm64.tar.gz"
            manifest = (hashlib.sha256(archive).hexdigest() + "  " + asset).encode()
            with patch.object(obot, "CACHE", Path(directory)), \
                    patch.object(obot.platform, "system", return_value="Linux"), \
                    patch.object(obot.platform, "machine", return_value="arm64"), \
                    patch.object(obot, "download", side_effect=[manifest, archive]) as download:
                binary = obot.cli("v0.26.3")
                self.assertEqual(binary.read_bytes(), b"test executable")
                self.assertEqual(binary.stat().st_mode & 0o777, 0o755)
                self.assertEqual(obot.cli("v0.26.3"), binary)
                self.assertEqual(download.call_count, 2)

    def test_archive_cannot_extract_paths_or_symlinks(self):
        for archive in [bundle("../obot"), bundle("/obot")]:
            with self.assertRaises(obot.SetupError):
                obot.release_binary(archive, "archive.tar.gz", "obot")
        stream = io.BytesIO()
        with tarfile.open(fileobj=stream, mode="w:gz") as archive:
            member = tarfile.TarInfo("obot")
            member.type = tarfile.SYMTYPE
            member.linkname = "/etc/passwd"
            archive.addfile(member)
        with self.assertRaises(obot.SetupError):
            obot.release_binary(stream.getvalue(), "archive.tar.gz", "obot")


class SourceTests(ToolingTest):
    def test_existing_checkout_is_preserved(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            for args in [
                ["git", "init", "-q", path],
                ["git", "-C", path, "config", "user.name", "Test"],
                ["git", "-C", path, "config", "user.email", "test@example.com"],
            ]:
                subprocess.run(args, check=True, capture_output=True)
            (path / "file").write_text("original")
            subprocess.run(["git", "-C", path, "add", "file"], check=True)
            subprocess.run(["git", "-C", path, "commit", "-qm", "test"], check=True)
            commit = subprocess.check_output(["git", "-C", path, "rev-parse", "HEAD"], text=True).strip()
            (path / "file").write_text("local edits")
            actual_path, actual_commit = obot.source("main", refresh=True, directory=path)
            self.assertEqual(actual_path, path.resolve())
            self.assertEqual(actual_commit, commit)
            self.assertEqual((path / "file").read_text(), "local edits")

    def test_managed_refresh_refuses_local_changes(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "sources" / "main"
            path.mkdir(parents=True)
            with patch.object(obot, "CACHE", Path(directory)), \
                    patch.object(obot, "run", return_value=" M file") as run:
                with self.assertRaisesRegex(obot.SetupError, "local changes"):
                    obot.source("main", refresh=True)
                self.assertEqual(run.call_count, 1)


class IsolationTests(ToolingTest):
    def test_http_failures_keep_status_and_do_not_expose_response_body(self):
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(401)
                self.end_headers()
                self.wfile.write(b"private-response-value")

            def log_message(self, *args):
                pass

        server = HTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with self.assertRaises(obot.HTTPFailure) as caught:
                obot.API(f"http://127.0.0.1:{server.server_port}").request("/api/me")
            self.assertEqual(caught.exception.status, 401)
            self.assertNotIn("private-response-value", str(caught.exception))
        finally:
            server.shutdown()
            thread.join()
            server.server_close()

    def test_exec_accepts_user_options_and_preserves_child_arguments(self):
        with patch.object(obot, "load", return_value={}), \
                patch.object(obot, "environment_for", return_value={}) as environment, \
                patch.object(obot.subprocess, "run", return_value=subprocess.CompletedProcess([], 0)) as run:
            self.assertEqual(obot.main(["exec", "dev", "--email", "user@example.com",
                                       "--", "go", "test", "-run", "TestAcc"]), 0)
            environment.assert_called_once_with([{}], "user@example.com")
            self.assertEqual(run.call_args.args[0], ["go", "test", "-run", "TestAcc"])

    def test_log_artifacts_redact_setup_tokens_and_api_keys(self):
        with tempfile.TemporaryDirectory() as directory:
            state = {"name": "dev", "container": "test-container"}
            data = {"Image": "sha256:image", "Config": {"Env": [
                "OBOT_SETUP_TOKEN=private-setup-value", "POSTGRES_PASSWORD=obot"]}}
            image = {"Config": {"Env": ["POSTGRES_PASSWORD=obot"]}}
            output = subprocess.CompletedProcess(
                [], 0, "private-setup-value ok1-1-2-privatekey obot started", "")
            with patch.object(obot, "CACHE", Path(directory)), \
                    patch.object(obot, "load", return_value=state), \
                    patch.object(obot, "owned", return_value=data), \
                    patch.object(obot, "inspect", return_value=image), \
                    patch.object(obot.subprocess, "run", return_value=output):
                text = obot.logs("dev")
            self.assertNotIn("private-setup-value", text)
            self.assertNotIn("ok1-1-2-privatekey", text)
            # The image's public default password is not a secret to redact.
            self.assertIn("obot started", text)
            self.assertEqual((Path(directory) / "logs" / "dev.log").read_text(), text)

    def test_log_artifacts_redact_a_password_overriding_the_image_default(self):
        with tempfile.TemporaryDirectory() as directory:
            data = {"Image": "sha256:image", "Config": {"Env": ["POSTGRES_PASSWORD=custom-secret"]}}
            image = {"Config": {"Env": ["POSTGRES_PASSWORD=obot"]}}
            output = subprocess.CompletedProcess([], 0, "custom-secret", "")
            with patch.object(obot, "CACHE", Path(directory)), \
                    patch.object(obot, "load", return_value={"name": "dev", "container": "c"}), \
                    patch.object(obot, "owned", return_value=data), \
                    patch.object(obot, "inspect", return_value=image), \
                    patch.object(obot.subprocess, "run", return_value=output):
                self.assertNotIn("custom-secret", obot.logs("dev"))

    def test_cleanup_refuses_foreign_containers_and_preserves_metadata(self):
        state = {"name": "dev", "container": "foreign", "volume": "foreign-data"}
        with patch.object(obot, "load", return_value=state), \
                patch.object(obot, "inspect", return_value={"Config": {"Labels": {obot.LABEL: "another-project"}}}), \
                patch.object(obot, "run") as run, \
                patch.object(obot.shutil, "rmtree") as remove:
            with self.assertRaisesRegex(obot.SetupError, "ownership label"):
                obot.stop("dev")
            run.assert_not_called()
            remove.assert_not_called()

    def test_startup_restarts_once_only_for_the_postgres_race(self):
        state = {"name": "dev", "container": "obot-dev", "url": "http://127.0.0.1:1"}
        stopped = {"State": {"Running": False}}
        race = subprocess.CompletedProcess([], 0, "", "Failed to connect: " + obot.POSTGRES_STARTUP_RACE)
        other = subprocess.CompletedProcess([], 0, "", "panic: invalid configuration")
        for name, logs, starts in (("race", race, 1), ("other failure", other, 0)):
            with self.subTest(name), \
                    patch.object(obot, "owned", return_value=stopped), \
                    patch.object(obot.subprocess, "run", return_value=logs), \
                    patch.object(obot, "run") as run:
                # The container stays stopped, so even the race retries only once.
                with self.assertRaisesRegex(obot.SetupError, "stopped during startup"):
                    obot.wait_for(state, 5)
                self.assertEqual(run.call_count, starts)
                if starts:
                    run.assert_called_once_with(["docker", "start", "obot-dev"])

    def test_each_instance_receives_its_own_credentials(self):
        states = [
            {"name": "one", "url": "http://127.0.0.1:10001", "email": "one@example.com", "ref": "main"},
            {"name": "two", "url": "http://127.0.0.1:10002", "email": "two@example.com", "ref": "main"},
        ]
        with patch.object(obot, "token", side_effect=["first-key", "second-key"]), \
                patch.object(obot, "cli", return_value=Path("/test/bin/obot")):
            environment = obot.environment_for(states)
        connections = json.loads(environment["OBOT_TEST_INSTANCES"])
        self.assertEqual(connections[0]["token"], "first-key")
        self.assertEqual(connections[1]["token"], "second-key")
        self.assertNotEqual(connections[0]["base_url"], connections[1]["base_url"])
        self.assertEqual(environment["OBOT_TOKEN"], "first-key")
        self.assertEqual(environment["OBOT_TEST_EMAIL"], "one@example.com")

    def test_instance_names_and_refs_cannot_escape_cache(self):
        for name in ["../dev", "/tmp/dev", "dev/name", "", "x" * 41]:
            with self.assertRaises(argparse.ArgumentTypeError):
                obot.valid_name(name)
        for ref in ["../main", "main;echo bad", "refs/heads/main", "v1.2.3/../other"]:
            with self.assertRaises(argparse.ArgumentTypeError):
                obot.valid_ref(ref)


class TerraformTests(ToolingTest):
    def test_named_instances_keep_independent_state_and_override_shell_target(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "dev.tfrc").touch()
            example = root / "examples" / "dev"
            example.mkdir(parents=True)
            (example / "main.tf").write_text('provider "obot" {}')
            with patch.object(obot, "ROOT", root), \
                    patch.object(obot, "CACHE", root / ".obot"), \
                    patch.object(obot, "load", side_effect=lambda name: {"name": name}), \
                    patch.object(obot, "environment_for", side_effect=lambda states: {
                        "OBOT_TOKEN": states[0]["name"] + "-key",
                        "TF_CLI_CONFIG_FILE": "/wrong/config",
                        "TF_DATA_DIR": "/wrong/data"}), \
                    patch.object(obot.subprocess, "run",
                                 return_value=subprocess.CompletedProcess([], 7)) as run:
                self.assertEqual(obot.main(["terraform", "dev", "plan", "--", "-no-color"]), 7)
                self.assertEqual(obot.main(["terraform", "release", "plan-destroy"]), 7)
            first, second = run.call_args_list
            for call, name in [(first, "dev"), (second, "release")]:
                command, environment = call.args[0], call.kwargs["env"]
                self.assertEqual(command[1], f"-chdir={root / '.obot' / 'terraform' / name}")
                self.assertEqual((root / ".obot" / "terraform" / name / "main.tf").resolve(), example / "main.tf")
                self.assertEqual(environment["TF_DATA_DIR"], str(root / ".obot" / "terraform" / name / ".terraform"))
                self.assertEqual(environment["TF_CLI_CONFIG_FILE"], str(root / "dev.tfrc"))
                self.assertEqual(environment["OBOT_TOKEN"], name + "-key")
            self.assertEqual(second.args[0][2:4], ["plan", "-destroy"])
            self.assertEqual(first.args[0][-1], "-no-color")


if __name__ == "__main__":
    unittest.main()
