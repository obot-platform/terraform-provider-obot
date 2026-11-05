#!/usr/bin/env python3
"""End-to-end and upgrade tests for the compiled provider against real Obot.

Unlike the acceptance tests, which run the provider in-process, these build
the provider binary and drive tests/e2e with the Terraform CLI, as a user's
pipeline would:

  e2e      create, update, refresh, re-import by ID and by identity, destroy,
           then verify through the Obot API that only managed objects changed.
  upgrade  apply on one Obot version, upgrade the server in place on the same
           data volume, and require an empty plan before updating and
           destroying on the new version.

Any non-empty plan where none is expected fails with the plan printed.
"""
import argparse
from contextlib import ExitStack
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import urllib.error
import uuid

import catalog_fixture
import obot

SCENARIO = obot.ROOT / "tests" / "e2e"
PUBLIC_ENTRY = "Extra Tool"
PRIVATE_ENTRY = "Private Tool"
SEEDED_RULE = "acr1-everything"

# Importing a resource by ID or identity must reproduce the configuration
# exactly, except for write-only values Obot can never return: the Git
# credential re-sends its token once after import, as documented.
IMPORT_UPDATES = {"obot_git_credential": {"token_wo", "token_wo_version"}}

# Paths that report a deleted object as 404, by resource type.
API_PATHS = {
    "obot_mcp_tunnel": "/api/mcp-tunnels/{id}",
    "obot_git_credential": "/api/git-credentials/{id}",
    "obot_mcp_access_rule": "/api/mcp-catalogs/default/access-control-rules/{id}",
}


class E2EFailure(Exception):
    pass


def managed_resources(state):
    """Managed resources from `terraform show -json` state output."""
    module = (state.get("values") or {}).get("root_module") or {}
    return [r for r in module.get("resources", []) if r.get("mode") == "managed"]


def import_target(resource, form):
    """The import ID or identity for a resource, in the requested form."""
    values = resource["values"]
    if resource["type"] == "obot_mcp_catalog_source":
        key, value = "url", values["url"]
    else:
        key, value = "id", values["id"]
    if form == "identity":
        return {key: value}
    if resource["type"] == "obot_mcp_access_rule":
        return f"{values.get('catalog_id') or 'default'}/{value}"
    return value


def import_blocks(resources, form):
    blocks = []
    for resource in resources:
        target = import_target(resource, form)
        if form == "identity":
            fields = ", ".join(f"{key} = {json.dumps(value)}" for key, value in target.items())
            line = f"  identity = {{ {fields} }}"
        else:
            line = f"  id = {json.dumps(target)}"
        blocks.append(f"import {{\n  to = {resource['address']}\n{line}\n}}\n")
    return "\n".join(blocks)


def changed_attributes(change):
    before, after = change.get("before") or {}, change.get("after") or {}
    unknown = change.get("after_unknown") or {}
    return {key for key in set(before) | set(after) | set(unknown)
            if before.get(key) != after.get(key) or unknown.get(key)}


def import_problems(plan, addresses):
    """Describe how an import plan differs from a pure adoption.

    Every address must be imported, nothing may be created, replaced, or
    destroyed, and only IMPORT_UPDATES may change.
    """
    problems = []
    seen = set()
    for change in plan.get("resource_changes", []):
        if change.get("mode") != "managed":
            continue
        address, details = change["address"], change.get("change", {})
        actions = details.get("actions", [])
        if details.get("importing") is not None:
            seen.add(address)
        else:
            problems.append(f"{address} was not imported")
        if actions == ["no-op"]:
            continue
        allowed = IMPORT_UPDATES.get(change.get("type"), set())
        changed = changed_attributes(details)
        if actions == ["update"] and changed <= allowed:
            continue
        problems.append(f"{address} plans {'/'.join(actions)} of {', '.join(sorted(changed)) or 'nothing'}")
    for address in sorted(set(addresses) - seen):
        problems.append(f"{address} is missing from the import plan")
    return problems


class Workspace:
    """A Terraform working directory for tests/e2e with its own state."""

    def __init__(self, path, cli_config, connection, variables):
        self.path = path
        for source in SCENARIO.glob("*.tf"):
            (path / source.name).write_text(source.read_text())
        self.environment = os.environ.copy()
        self.environment.pop("TF_REATTACH_PROVIDERS", None)
        self.environment.update({
            "TF_CLI_CONFIG_FILE": str(cli_config),
            "TF_DATA_DIR": str(path / ".terraform"),
            "TF_IN_AUTOMATION": "1",
            "TF_INPUT": "0",
            "CHECKPOINT_DISABLE": "1",
            "OBOT_BASE_URL": connection["base_url"],
            "OBOT_TOKEN": connection["token"],
        })
        for key, value in variables.items():
            self.environment["TF_VAR_" + key] = str(value)

    def terraform(self, *arguments, capture=False, check=True):
        command = ["terraform", f"-chdir={self.path}", *arguments]
        result = subprocess.run(command, env=self.environment, text=True,
                                stdout=subprocess.PIPE if capture else sys.stderr)
        if check and result.returncode:
            raise E2EFailure(f"terraform {arguments[0]} failed (exit {result.returncode})")
        return result

    def apply(self, stage, generation=1):
        obot.note(f"== {stage}")
        self.terraform("apply", "-auto-approve", "-no-color", f"-var=generation={generation}")

    def expect_empty_plan(self, stage, generation=1):
        obot.note(f"== {stage}: expecting an empty plan")
        plan = self.path / "check.tfplan"
        result = self.terraform("plan", "-detailed-exitcode", "-no-color", f"-var=generation={generation}",
                                "-out", str(plan), check=False)
        if result.returncode == 2:
            self.terraform("show", "-no-color", str(plan), check=False)
            raise E2EFailure(f"{stage}: Terraform planned changes against an unchanged configuration")
        if result.returncode:
            raise E2EFailure(f"{stage}: terraform plan failed (exit {result.returncode})")

    def state(self):
        return json.loads(self.terraform("show", "-json", capture=True).stdout)

    def reimport(self, form, generation):
        """Discard state, adopt every object again, and require a clean result."""
        stage = f"re-import by {form}"
        resources = managed_resources(self.state())
        if not resources:
            raise E2EFailure(f"{stage}: no managed resources in state")
        obot.note(f"== {stage}: importing {len(resources)} resources into empty state")
        (self.path / "terraform.tfstate").replace(self.path / f"before-{form}-import.tfstate")
        imports = self.path / "imports.tf"
        imports.write_text(import_blocks(resources, form))
        plan = self.path / "import.tfplan"
        self.terraform("plan", "-no-color", f"-var=generation={generation}", "-out", str(plan))
        decoded = json.loads(self.terraform("show", "-json", str(plan), capture=True).stdout)
        problems = import_problems(decoded, [r["address"] for r in resources])
        if problems:
            self.terraform("show", "-no-color", str(plan), check=False)
            raise E2EFailure(f"{stage}: " + "; ".join(problems))
        self.terraform("apply", "-auto-approve", "-no-color", str(plan))
        imports.unlink()
        self.expect_empty_plan(f"{stage}: after import", generation)

    def destroy(self, generation):
        obot.note("== destroy")
        self.terraform("destroy", "-auto-approve", "-no-color", f"-var=generation={generation}")


def verify_destroyed(api, resources, initial_source):
    """Confirm through the API that managed objects, and only they, are gone."""
    for resource in resources:
        template = API_PATHS.get(resource["type"])
        if not template:
            continue
        path = template.format(id=resource["values"]["id"])
        try:
            api.request(path)
        except obot.HTTPFailure as error:
            if error.status == 404:
                continue
            raise
        raise E2EFailure(f"{resource['address']} still exists in Obot after destroy")
    try:
        api.request(API_PATHS["obot_mcp_access_rule"].format(id=SEEDED_RULE))
    except obot.HTTPFailure as error:
        raise E2EFailure(f"Destroy removed or hid the seeded {SEEDED_RULE} policy "
                         f"(HTTP {error.status})") from None
    sources = api.request("/api/mcp-catalogs/default").get("sourceURLs") or []
    if sources != [initial_source]:
        raise E2EFailure(f"Default catalog sources after destroy are {sources}, "
                         f"expected only the shipped source")
    obot.note("Obot API confirms managed objects were removed and defaults were kept.")


def scenario_variables(state, fixture):
    return {
        "expected_email": state["email"],
        "public_source": fixture["extra"],
        "public_entry": PUBLIC_ENTRY,
        "private_source": fixture["private"],
        "private_entry": PRIVATE_ENTRY,
        "private_host": fixture["private_host"],
        "private_token": fixture["private_token"],
    }


def connection(state):
    return {"base_url": state["url"] + "/api", "token": obot.token(state)}


def run_e2e(ref, workdir, cli_config, fixture, launch):
    prepared = obot.prepare(ref)
    obot.cli(ref, revision=prepared["cli_revision"])
    state = launch(ref, prepared)
    access = connection(state)
    workspace = Workspace(workdir, cli_config, access, scenario_variables(state, fixture))
    workspace.apply("create", 1)
    workspace.expect_empty_plan("after create", 1)
    workspace.apply("update every object in place", 2)
    workspace.expect_empty_plan("after update", 2)
    workspace.terraform("apply", "-refresh-only", "-auto-approve", "-no-color", "-var=generation=2")
    workspace.expect_empty_plan("after refresh", 2)
    workspace.reimport("id", 2)
    workspace.reimport("identity", 2)
    resources = managed_resources(workspace.state())
    workspace.destroy(2)
    verify_destroyed(obot.API(state["url"], access["token"]), resources, fixture["initial"])


def run_upgrade(from_ref, to_ref, workdir, cli_config, fixture, launch, timeout):
    prepared = obot.prepare(from_ref)
    obot.cli(from_ref, revision=prepared["cli_revision"])
    state = launch(from_ref, prepared)
    access = connection(state)
    workspace = Workspace(workdir, cli_config, access, scenario_variables(state, fixture))
    workspace.apply(f"create on {state['server_version']}", 1)
    workspace.expect_empty_plan(f"before upgrade, on {state['server_version']}", 1)

    target = obot.prepare(to_ref)
    obot.cli(to_ref, revision=target["cli_revision"])
    obot.upgrade(state, target, timeout=timeout, catalog_test=fixture)
    me = obot.API(state["url"], access["token"]).request("/api/me")
    if me.get("email") != state["email"]:
        raise E2EFailure("The API key created before the upgrade no longer identifies its owner")

    workspace.expect_empty_plan(f"after upgrade to {state['server_version']}", 1)
    workspace.apply(f"update every object on {state['server_version']}", 2)
    workspace.expect_empty_plan("after update on the upgraded server", 2)
    resources = managed_resources(workspace.state())
    workspace.destroy(2)
    verify_destroyed(obot.API(state["url"], access["token"]), resources, fixture["initial"])


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    p = sub.add_parser("e2e", help="Run the end-to-end scenario on one Obot version")
    p.add_argument("--ref", type=obot.valid_ref, default="main")
    p = sub.add_parser("upgrade", help="Apply on one Obot version and plan after upgrading to another")
    p.add_argument("--from", dest="from_ref", type=obot.valid_ref, required=True)
    p.add_argument("--to", dest="to_ref", type=obot.valid_ref, default="main")
    for p in sub.choices.values():
        p.add_argument("--keep", action="store_true",
                       help="Keep the instance and Terraform directory for diagnosis")
        p.add_argument("--timeout", type=int, default=180)
    args = parser.parse_args(argv)

    names = []
    with ExitStack() as stack:
        fixture = stack.enter_context(catalog_fixture.serve())
        workdir = Path(tempfile.mkdtemp(prefix=f"obot-{args.command}-"))
        cli_config = obot.build_provider(workdir / "provider")
        scenario = workdir / "scenario"
        scenario.mkdir()

        def launch(ref, prepared):
            name = f"{args.command}-{uuid.uuid4().hex[:8]}"
            names.append(name)
            return obot.start(name, ref, timeout=args.timeout, catalog_test=fixture, prepared=prepared)

        try:
            if args.command == "e2e":
                run_e2e(args.ref, scenario, cli_config, fixture, launch)
                obot.note(f"End-to-end scenario passed on {args.ref}.")
            else:
                run_upgrade(args.from_ref, args.to_ref, scenario, cli_config, fixture, launch, args.timeout)
                obot.note(f"Upgrade from {args.from_ref} to {args.to_ref} kept Terraform state consistent.")
            return 0
        except BaseException:
            for name in names:
                if obot.state_path(name).exists():
                    try:
                        obot.logs(name)
                    except (obot.SetupError, OSError):
                        pass
            raise
        finally:
            if args.keep:
                obot.note(f"Kept instances {', '.join(names)} and Terraform directory {scenario}")
            else:
                for name in names:
                    if obot.state_path(name).exists():
                        obot.stop(name)
                # State holds a sensitive tunnel token; do not leave it behind.
                shutil.rmtree(workdir, ignore_errors=True)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except E2EFailure as error:
        obot.note(f"FAILED: {error}")
        sys.exit(1)
    except (obot.SetupError, urllib.error.URLError, TimeoutError, ConnectionError, OSError,
            subprocess.TimeoutExpired) as error:
        obot.note(f"Error: {error}")
        sys.exit(1)
