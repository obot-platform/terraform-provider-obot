#!/usr/bin/env python3
"""Resolve the Obot versions CI tests from obot-versions.json.

Each entry in "supported" is a minor series (v0.27), meaning its newest
stable patch, or an exact release tag (v0.26.3). main is always tested.

With --discover, as the nightly run uses, a stable Obot release newer than
every listed series is tested too and reported as drift, so a lagging config
still gets early warning instead of silently missing a release.
"""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
CONFIG = ROOT / "obot-versions.json"
OBOT_REPO = "https://github.com/obot-platform/obot.git"
STABLE = re.compile(r"^v(\d+)\.(\d+)\.(\d+)$")
SERIES = re.compile(r"^v(\d+)\.(\d+)$")


class ConfigError(Exception):
    pass


def version(tag):
    match = STABLE.match(tag)
    return tuple(int(part) for part in match.groups()) if match else None


def remote_tags(remote):
    """Tag names on a Git remote; needs no API token and has no rate limit."""
    output = subprocess.run(["git", "ls-remote", "--tags", "--refs", remote], text=True,
                            capture_output=True, check=True, timeout=120).stdout
    return [line.split("refs/tags/", 1)[1] for line in output.splitlines() if "refs/tags/" in line]


def newest_stable(tags):
    stable = [tag for tag in tags if version(tag)]
    return max(stable, key=version) if stable else None


def load_config(path=CONFIG):
    try:
        config = json.loads(Path(path).read_text())
    except (OSError, ValueError) as error:
        raise ConfigError(f"Cannot read {path}: {error}") from None
    entries = config.get("supported")
    if not isinstance(entries, list) or not entries:
        raise ConfigError(f"{path} must list at least one supported version")
    for entry in entries:
        if not isinstance(entry, str) or not (SERIES.match(entry) or STABLE.match(entry)):
            raise ConfigError(f"Unsupported entry {entry!r}: use a series like v0.27 or a tag like v0.26.3")
    if len(set(entries)) != len(entries):
        raise ConfigError(f"{path} lists a version more than once")
    return entries


def resolve(entries, tags, discover=False):
    """Map config entries to release tags and plan the test matrix."""
    stable = sorted({tag for tag in tags if version(tag)}, key=version)
    releases, warnings = [], []
    for entry in entries:
        if STABLE.match(entry):
            if entry not in stable:
                raise ConfigError(f"{entry} is not a published stable Obot release")
            ref = entry
            series = version(entry)[:2]
            latest = [tag for tag in stable if version(tag)[:2] == series][-1]
            if latest != entry:
                warnings.append(f"{entry} is pinned, but {latest} is the newest v{series[0]}.{series[1]} release.")
        else:
            series = tuple(int(part) for part in SERIES.match(entry).groups())
            matches = [tag for tag in stable if version(tag)[:2] == series]
            if not matches:
                raise ConfigError(f"No stable Obot release exists for series {entry}")
            ref = matches[-1]
        releases.append({"ref": ref, "entry": entry, "listed": True})

    newest = stable[-1] if stable else None
    listed_series = {version(r["ref"])[:2] for r in releases}
    if discover and newest and version(newest)[:2] > max(listed_series):
        major, minor = version(newest)[:2]
        releases.append({"ref": newest, "entry": f"v{major}.{minor}", "listed": False})
        warnings.append(f"Obot {newest} is released, but obot-versions.json does not list v{major}.{minor}. "
                        f"Nightly tests it anyway; add v{major}.{minor} to obot-versions.json and "
                        f"remove series that are no longer supported.")

    ordered = []
    for release in sorted(releases, key=lambda r: version(r["ref"])):
        if release["ref"] not in [r["ref"] for r in ordered]:
            ordered.append(release)
    refs = [r["ref"] for r in ordered] + ["main"]
    # Upgrade each tested release to the next, as operators move between
    # releases, and the newest release to main, which catches migrations
    # before they ship.
    upgrades = [{"from": a, "to": b} for a, b in zip(refs, refs[1:])]
    return {"releases": ordered, "refs": refs, "upgrades": upgrades,
            "newest_release": newest, "warnings": warnings}


def write_github_output(plan, provider_release):
    path = os.environ.get("GITHUB_OUTPUT")
    if not path:
        raise ConfigError("GITHUB_OUTPUT is not set; --github-output only works in GitHub Actions")
    with open(path, "a") as handle:
        handle.write(f"refs={json.dumps(plan['refs'])}\n")
        handle.write(f"releases={json.dumps([r['ref'] for r in plan['releases']])}\n")
        handle.write(f"upgrades={json.dumps(plan['upgrades'])}\n")
        handle.write(f"provider_release={provider_release or ''}\n")
        handle.write(f"warnings={json.dumps(plan['warnings'])}\n")
    for warning in plan["warnings"]:
        print(f"::warning title=Obot versions::{warning}")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    for name in ("show", "matrix"):
        p = sub.add_parser(name)
        p.add_argument("--config", type=Path, default=CONFIG)
        p.add_argument("--discover", action="store_true",
                       help="Also test a stable release newer than every listed series")
        if name == "matrix":
            p.add_argument("--provider-remote",
                           help="Git remote to find the newest released provider tag on")
            p.add_argument("--github-output", action="store_true")
    args = parser.parse_args(argv)
    try:
        plan = resolve(load_config(args.config), remote_tags(OBOT_REPO), args.discover)
        if args.command == "show":
            for release in plan["releases"]:
                note = "listed" if release["listed"] else "NOT LISTED (discovered)"
                print(f"{release['entry']:>8}  ->  {release['ref']:<10} {note}")
            print(f"{'main':>8}  ->  main")
            for upgrade in plan["upgrades"]:
                print(f"upgrade   {upgrade['from']} -> {upgrade['to']}")
            for warning in plan["warnings"]:
                print("warning: " + warning, file=sys.stderr)
            return 0
        provider_release = newest_stable(remote_tags(args.provider_remote)) if args.provider_remote else None
        if args.github_output:
            write_github_output(plan, provider_release)
        print(json.dumps({**plan, "provider_release": provider_release}, indent=2))
        return 0
    except (ConfigError, subprocess.SubprocessError) as error:
        print(f"Error: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
