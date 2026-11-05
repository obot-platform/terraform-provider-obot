"""Behavior checks for nightly leg running and reporting; no network is required."""
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

import nightly


def leg(name, kind, refs, status, revision=None, version=None):
    provenance = []
    for ref in refs:
        provenance.append({"ref": ref, "server_version": version or f"{ref}+abc",
                           "server_revision": revision if ref == "main" else None,
                           "image_digests": [f"ghcr.io/obot-platform/obot@sha256:{'0' * 64}"]})
    return {"name": name, "kind": kind, "refs": refs, "status": status,
            "attempts": [{"exit": 0, "seconds": 1}], "provenance": provenance}


class LegTests(unittest.TestCase):
    def run_leg(self, codes):
        calls = []

        def runner(command, env):
            calls.append(env["OBOT_TEST_PROVENANCE"])
            Path(env["OBOT_TEST_PROVENANCE"]).write_text(json.dumps({"ref": "main"}) + "\n")
            return subprocess.CompletedProcess(command, codes[len(calls) - 1])

        with tempfile.TemporaryDirectory() as directory:
            result = nightly.run_leg("e2e / main", "e2e", ["main"], ["true"], Path(directory),
                                     runner=runner)
            written = json.loads((Path(directory) / "result-e2e-main.json").read_text())
        self.assertEqual(result, written)
        return result, calls

    def test_pass_on_first_attempt(self):
        result, calls = self.run_leg([0])
        self.assertEqual((result["status"], len(calls)), ("passed", 1))
        self.assertEqual(result["provenance"], [{"ref": "main"}])

    def test_pass_on_retry_is_reported_as_flaky(self):
        result, calls = self.run_leg([1, 0])
        self.assertEqual((result["status"], len(calls)), ("flaky", 2))
        self.assertNotEqual(calls[0], calls[1])

    def test_two_failures_fail(self):
        result, _ = self.run_leg([1, 1])
        self.assertEqual(result["status"], "failed")
        self.assertEqual([a["exit"] for a in result["attempts"]], [1, 1])


class ReportTests(unittest.TestCase):
    def test_passing_run_records_main_as_last_good(self):
        report = nightly.summarize([leg("e2e / main", "e2e", ["main"], "passed", "new")],
                                   {"e2e": "success"}, [], {"last_good_main": "old"}, "")
        self.assertEqual((report["status"], report["last_good_main"]), ("passed", "new"))

    def test_failing_main_keeps_last_good_and_links_the_commit_range(self):
        report = nightly.summarize([leg("e2e / main", "e2e", ["main"], "failed", "new"),
                                    leg("e2e / v0.27.0", "e2e", ["v0.27.0"], "passed")],
                                   {"e2e": "failure"}, [], {"last_good_main": "old"}, "https://run")
        self.assertEqual((report["status"], report["last_good_main"]), ("failed", "old"))
        markdown = nightly.render(report)
        self.assertIn("obot-platform/obot/compare/old...new", markdown)
        self.assertIn("FAILING", markdown)

    def test_unchanged_main_points_away_from_obot(self):
        report = nightly.summarize([leg("e2e / main", "e2e", ["main"], "failed", "same")],
                                   {}, [], {"last_good_main": "same"}, "")
        self.assertIn("suspect a provider, test, or environment change", nightly.render(report))

    def test_flaky_legs_pass_but_are_visible(self):
        report = nightly.summarize([leg("e2e / main", "e2e", ["main"], "flaky", "r")], {}, [], None, "")
        self.assertEqual(report["status"], "passed")
        self.assertIn("flaky", nightly.render(report))

    def test_job_that_ended_without_results_fails_the_report(self):
        report = nightly.summarize([leg("e2e / main", "e2e", ["main"], "passed", "r")],
                                   {"plan": "success", "upgrade": "cancelled", "e2e": "success",
                                    "released-provider": "skipped"}, [], None, "")
        self.assertEqual((report["status"], report["broken_jobs"]), ("failed", ["upgrade"]))
        self.assertIn("job ended before reporting", nightly.render(report))

    def test_upgrade_versions_and_migration_are_shown(self):
        result = leg("upgrade / v0.27.0 → main", "upgrade", ["v0.27.0", "main"], "passed", "r")
        result["provenance"][1]["postgres_migration"] = "17->18"
        self.assertIn("v0.27.0+abc → main+abc (PostgreSQL 17->18 migrated)", nightly.leg_versions(result))

    def test_version_warnings_are_reported(self):
        report = nightly.summarize([], {}, ["Obot v0.28.0 is released"], None, "")
        self.assertIn("Obot v0.28.0 is released", nightly.render(report))


class FakeGitHub(nightly.GitHub):
    def __init__(self, open_number=None):
        super().__init__()
        self.open_number = open_number
        self.calls = []

    def open_issue(self, label):
        return self.open_number

    def ensure_label(self, label, description):
        self.calls.append(("label", label))

    def with_body(self, body, *arguments):
        self.calls.append(arguments)


class IssueTests(unittest.TestCase):
    def manage(self, github, active, comment=True):
        return nightly.manage_issue(github, "nightly-failure", "Failing", "desc", active, "body", "fixed",
                                    comment=comment)

    def test_failure_opens_one_issue(self):
        github = FakeGitHub()
        self.assertEqual(self.manage(github, True), "opened an issue")
        self.assertEqual(github.calls, [("label", "nightly-failure"),
                                        ("issue", "create", "--title", "Failing", "--label", "nightly-failure")])

    def test_repeated_failure_comments_on_the_open_issue(self):
        github = FakeGitHub(7)
        self.assertEqual(self.manage(github, True), "commented on #7")
        self.assertEqual(github.calls, [("issue", "comment", "7")])

    def test_drift_issue_is_not_commented_every_night(self):
        github = FakeGitHub(7)
        self.assertEqual(self.manage(github, True, comment=False), "#7 already open")
        self.assertEqual(github.calls, [])

    def test_recovery_closes_the_issue(self):
        github = FakeGitHub(7)
        self.assertEqual(self.manage(github, False), "closed #7")
        self.assertEqual(github.calls, [("issue", "close", "7", "--comment")])

    def test_passing_without_issue_does_nothing(self):
        github = FakeGitHub()
        self.assertEqual(self.manage(github, False), "nothing to do")
        self.assertEqual(github.calls, [])

    def test_previous_report_skips_the_current_run_and_runs_without_reports(self):
        downloads = []

        def runner(command, text, capture_output):
            if command[1:3] == ["run", "list"]:
                return subprocess.CompletedProcess(command, 0, json.dumps(
                    [{"databaseId": 3}, {"databaseId": 2}, {"databaseId": 1}]), "")
            run_id, target = command[3], Path(command[command.index("--dir") + 1])
            downloads.append(run_id)
            if run_id == "2":
                return subprocess.CompletedProcess(command, 1, "", "no artifact")
            target.mkdir(parents=True)
            (target / "report.json").write_text(json.dumps({"last_good_main": "abc"}))
            return subprocess.CompletedProcess(command, 0, "", "")

        with tempfile.TemporaryDirectory() as directory:
            report = nightly.previous_report(nightly.GitHub(runner), "nightly.yml", "3", directory)
        self.assertEqual((report, downloads), ({"last_good_main": "abc"}, ["2", "1"]))


if __name__ == "__main__":
    unittest.main()
