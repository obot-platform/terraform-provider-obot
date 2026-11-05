#!/usr/bin/env python3
"""Run nightly compatibility legs and report their results.

  leg     Run one test command, retrying once, and record the result with the
          exact Obot images it tested. A leg that passes only on retry is
          reported as flaky rather than hidden or failed.
  report  Combine leg results into the run summary, compare main against the
          last Obot commit that passed, and open, update, or close the
          tracking issues.
"""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time

OBOT_REPO_URL = "https://github.com/obot-platform/obot"
FAILURE_LABEL = "nightly-failure"
FAILURE_TITLE = "Nightly Obot compatibility tests are failing"
DRIFT_LABEL = "obot-version-drift"
DRIFT_TITLE = "obot-versions.json is behind the latest Obot release"
REPORT_ARTIFACT = "nightly-report"
PASSING = ("passed", "flaky")


def slug(text):
    return re.sub(r"[^A-Za-z0-9._-]+", "-", text).strip("-")


# ---------------------------------------------------------------- legs

def run_leg(name, kind, refs, command, out, attempts=2, runner=subprocess.run):
    out.mkdir(parents=True, exist_ok=True)
    history = []
    status = "failed"
    for attempt in range(1, attempts + 1):
        provenance = out / f"{slug(name)}-attempt{attempt}.provenance.jsonl"
        environment = os.environ.copy()
        environment["OBOT_TEST_PROVENANCE"] = str(provenance)
        if attempt > 1:
            print(f"::warning title={name}::Attempt {attempt - 1} failed; retrying once to separate "
                  "flakes from real breaks", flush=True)
        started = time.monotonic()
        code = runner(command, env=environment).returncode
        history.append({"exit": code, "seconds": round(time.monotonic() - started)})
        if code == 0:
            status = "passed" if attempt == 1 else "flaky"
            break
    records = []
    if provenance.exists():
        records = [json.loads(line) for line in provenance.read_text().splitlines() if line.strip()]
    result = {"name": name, "kind": kind, "refs": refs, "status": status,
              "attempts": history, "provenance": records}
    (out / f"result-{slug(name)}.json").write_text(json.dumps(result, indent=2) + "\n")
    return result


# ---------------------------------------------------------------- report

def load_results(directory):
    return sorted((json.loads(path.read_text()) for path in Path(directory).rglob("result-*.json")),
                  key=lambda r: (r["kind"], r["name"]))


def main_revision(results):
    """The Obot main commit this run tested, from the image's own version."""
    for result in results:
        for record in result.get("provenance", []):
            if record.get("ref") == "main" and record.get("server_revision"):
                return record["server_revision"]
    return None


def summarize(results, jobs, warnings, previous, run_url):
    """Build the report record from leg results and the workflow's job results."""
    failed = [r for r in results if r["status"] not in PASSING]
    reported_kinds = {r["kind"] for r in results}
    # A job that timed out, was cancelled, or failed in setup writes no
    # failing leg result; it must still fail the report.
    broken_jobs = sorted(name for name, result in jobs.items()
                         if result in ("failure", "cancelled")
                         and not any(r["kind"] == name and r["status"] not in PASSING for r in results))
    main_legs = [r for r in results if "main" in r["refs"]]
    revision = main_revision(results)
    last_good = (previous or {}).get("last_good_main")
    main_ok = main_legs and all(r["status"] in PASSING for r in main_legs) and \
        not any(job for job in broken_jobs if job not in reported_kinds)
    if main_ok and revision:
        last_good = revision
    return {
        "run_url": run_url,
        "status": "failed" if failed or broken_jobs else "passed",
        "legs": results,
        "broken_jobs": broken_jobs,
        "warnings": warnings,
        "main_revision": revision,
        "last_good_main": last_good,
        "previous_last_good_main": (previous or {}).get("last_good_main"),
    }


def leg_versions(result):
    seen = []
    for record in result.get("provenance", []):
        text = record.get("server_version") or record.get("ref") or ""
        if record.get("postgres_migration"):
            text += f" (PostgreSQL {record['postgres_migration']} migrated)"
        if text and text not in seen:
            seen.append(text)
    return " → ".join(seen) or "not recorded"


def leg_digest(result):
    digests = []
    for record in result.get("provenance", []):
        for digest in record.get("image_digests") or []:
            short = digest.split("@")[-1][:19]
            if short not in digests:
                digests.append(short)
    return ", ".join(f"`{d}`" for d in digests) or "—"


def render(report):
    icon = {"passed": "✅", "flaky": "⚠️ flaky", "failed": "❌"}
    lines = [f"## Nightly Obot compatibility: {'passing' if report['status'] == 'passed' else 'FAILING'}", ""]
    if report["run_url"]:
        lines += [f"Run: {report['run_url']}", ""]
    lines += ["| Leg | Result | Attempts | Obot tested | Image digest |", "| --- | --- | --- | --- | --- |"]
    for leg in report["legs"]:
        attempts = len(leg["attempts"])
        lines.append(f"| {leg['name']} | {icon.get(leg['status'], leg['status'])} | {attempts} | "
                     f"{leg_versions(leg)} | {leg_digest(leg)} |")
    for job in report["broken_jobs"]:
        lines.append(f"| {job} | ❌ job ended before reporting (timeout, setup failure, or cancellation) | | | |")
    lines.append("")
    failing_main = [leg for leg in report["legs"] if "main" in leg["refs"] and leg["status"] not in PASSING]
    revision, good = report["main_revision"], report["previous_last_good_main"]
    if failing_main and revision:
        if good and good != revision:
            lines += [f"Obot main changed between the last passing run and this one: "
                      f"[{good}...{revision}]({OBOT_REPO_URL}/compare/{good}...{revision})", ""]
        elif good == revision:
            lines += [f"Obot main is unchanged since the last passing run ({revision}); "
                      "suspect a provider, test, or environment change.", ""]
        else:
            lines += [f"Obot main tested: [{revision}]({OBOT_REPO_URL}/commit/{revision}). "
                      "No earlier passing run is recorded to compare against.", ""]
    if report["warnings"]:
        lines += ["### Version configuration", ""] + [f"- {w}" for w in report["warnings"]] + [""]
    if report["status"] != "passed":
        lines += ["Reproduce a leg locally with the command in its job log, for example "
                  "`make test-e2e OBOT_REF=main` or `make test-upgrade UPGRADE_FROM=v0.27.0 UPGRADE_TO=main`. "
                  "Sanitized server logs are in each leg's artifact.", ""]
    return "\n".join(lines)


# ---------------------------------------------------------------- GitHub

class GitHub:
    """The few gh operations the report needs; replaceable in tests."""

    def __init__(self, runner=subprocess.run):
        self.runner = runner

    def gh(self, *arguments, capture=True):
        result = self.runner(["gh", *arguments], text=True, capture_output=capture)
        if result.returncode:
            raise RuntimeError(f"gh {arguments[0]} {arguments[1] if len(arguments) > 1 else ''} failed: "
                               f"{(result.stderr or '').strip()}")
        return result.stdout

    def open_issue(self, label):
        issues = json.loads(self.gh("issue", "list", "--label", label, "--state", "open",
                                    "--json", "number", "--limit", "1") or "[]")
        return issues[0]["number"] if issues else None

    def ensure_label(self, label, description):
        self.gh("label", "create", label, "--color", "B60205", "--description", description, "--force")

    def with_body(self, body, *arguments):
        with tempfile.NamedTemporaryFile("w", suffix=".md", delete=False) as handle:
            handle.write(body)
        try:
            return self.gh(*arguments, "--body-file", handle.name)
        finally:
            os.unlink(handle.name)


def manage_issue(github, label, title, description, active, body, resolved_message, comment=True):
    """Keep one open issue per label while a condition holds; close it after.

    With comment, each run that still matches adds its report to the open
    issue; otherwise the issue stays as opened until resolved.
    """
    number = github.open_issue(label)
    if active:
        if number:
            if not comment:
                return f"#{number} already open"
            github.with_body(body, "issue", "comment", str(number))
            return f"commented on #{number}"
        github.ensure_label(label, description)
        github.with_body(body, "issue", "create", "--title", title, "--label", label)
        return "opened an issue"
    if number:
        github.with_body(resolved_message, "issue", "close", str(number), "--comment")
        return f"closed #{number}"
    return "nothing to do"


def previous_report(github, workflow, current_run, directory):
    """The report artifact of the most recent earlier completed run, if any."""
    runs = json.loads(github.gh("run", "list", "--workflow", workflow, "--branch",
                                os.environ.get("GITHUB_REF_NAME", "main"), "--status", "completed",
                                "--limit", "10", "--json", "databaseId") or "[]")
    for run in runs:
        run_id = str(run["databaseId"])
        if run_id == str(current_run):
            continue
        target = Path(directory) / run_id
        try:
            github.gh("run", "download", run_id, "--name", REPORT_ARTIFACT, "--dir", str(target))
        except RuntimeError:
            continue
        path = target / "report.json"
        if path.exists():
            return json.loads(path.read_text())
    return None


def main(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    command = []
    if "--" in argv:
        separator = argv.index("--")
        argv, command = argv[:separator], argv[separator + 1:]
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    p = sub.add_parser("leg")
    p.add_argument("--name", required=True)
    p.add_argument("--kind", required=True)
    p.add_argument("--ref", action="append", dest="refs", required=True, help="Obot ref the leg tests")
    p.add_argument("--out", type=Path, required=True)
    p.add_argument("--attempts", type=int, default=2)
    p = sub.add_parser("report")
    p.add_argument("--results", type=Path, required=True)
    p.add_argument("--out", type=Path, required=True)
    p.add_argument("--jobs", default="{}", help="JSON of the workflow's needs context")
    p.add_argument("--warnings", default="[]", help="JSON list of version configuration warnings")
    p.add_argument("--run-url", default="")
    p.add_argument("--previous", action="store_true", help="Compare with the last completed run's report")
    p.add_argument("--manage-issues", action="store_true")
    args = parser.parse_args(argv)

    if args.command == "leg":
        if not command:
            parser.error("leg requires a command after --")
        result = run_leg(args.name, args.kind, args.refs, command, args.out, args.attempts)
        return 0 if result["status"] in PASSING else 1

    jobs = {name: value.get("result") for name, value in json.loads(args.jobs or "{}").items()}
    github = GitHub()
    previous = None
    if args.previous:
        try:
            with tempfile.TemporaryDirectory() as directory:
                previous = previous_report(github, "nightly.yml", os.environ.get("GITHUB_RUN_ID", ""), directory)
        except RuntimeError as error:
            print(f"::warning::Could not read the previous nightly report: {error}")
    report = summarize(load_results(args.results), jobs, json.loads(args.warnings or "[]"), previous, args.run_url)
    markdown = render(report)
    args.out.mkdir(parents=True, exist_ok=True)
    (args.out / "report.json").write_text(json.dumps(report, indent=2) + "\n")
    (args.out / "report.md").write_text(markdown)
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a") as handle:
            handle.write(markdown + "\n")
    print(markdown)
    if args.manage_issues:
        run = f" in {args.run_url}" if args.run_url else ""
        print("Failure issue: " + manage_issue(
            github, FAILURE_LABEL, FAILURE_TITLE, "Nightly Obot compatibility tests are failing",
            report["status"] != "passed", markdown, f"Nightly compatibility tests pass again{run}."))
        drift = "\n".join(["Nightly found Obot releases that obot-versions.json does not list. "
                           "They are tested anyway until the file is updated.", ""] +
                          [f"- {w}" for w in report["warnings"]] + ["", f"Run: {args.run_url}"])
        print("Drift issue: " + manage_issue(
            github, DRIFT_LABEL, DRIFT_TITLE, "obot-versions.json needs a newer Obot release",
            bool(report["warnings"]), drift, f"obot-versions.json is current again{run}.", comment=False))
    return 0 if report["status"] == "passed" else 1


if __name__ == "__main__":
    sys.exit(main())
