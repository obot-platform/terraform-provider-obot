"""Behavior checks for end-to-end test helpers; no Docker or Terraform is required."""
import unittest

import e2e
import obot

RESOURCES = [
    {"address": "obot_mcp_tunnel.e2e", "mode": "managed", "type": "obot_mcp_tunnel",
     "values": {"id": "mt1abc"}},
    {"address": "obot_mcp_catalog_source.public", "mode": "managed", "type": "obot_mcp_catalog_source",
     "values": {"id": "https://host:1/extra.yaml", "url": "https://host:1/extra.yaml"}},
    {"address": "obot_mcp_access_rule.e2e", "mode": "managed", "type": "obot_mcp_access_rule",
     "values": {"id": "acr1abc", "catalog_id": "default"}},
    {"address": "obot_git_credential.private", "mode": "managed", "type": "obot_git_credential",
     "values": {"id": "gc1abc"}},
]


def change(address, kind, actions, before=None, after=None, importing=True):
    details = {"actions": actions, "before": before or {}, "after": after or {}, "after_unknown": {}}
    if importing:
        details["importing"] = {"id": "x"}
    return {"address": address, "mode": "managed", "type": kind, "change": details}


class ImportBlockTests(unittest.TestCase):
    def test_import_ids_match_each_resource_documented_form(self):
        blocks = e2e.import_blocks(RESOURCES, "id")
        self.assertIn('to = obot_mcp_tunnel.e2e\n  id = "mt1abc"', blocks)
        self.assertIn('id = "https://host:1/extra.yaml"', blocks)
        self.assertIn('id = "default/acr1abc"', blocks)
        self.assertEqual(blocks.count("import {"), 4)

    def test_identities_use_each_resource_identity_attribute(self):
        blocks = e2e.import_blocks(RESOURCES, "identity")
        self.assertIn('identity = { url = "https://host:1/extra.yaml" }', blocks)
        self.assertIn('identity = { id = "acr1abc" }', blocks)
        self.assertIn('identity = { id = "gc1abc" }', blocks)

    def test_managed_resources_skip_data_sources(self):
        state = {"values": {"root_module": {"resources": RESOURCES + [
            {"address": "data.obot_current_user.me", "mode": "data", "type": "obot_current_user"}]}}}
        self.assertEqual(len(e2e.managed_resources(state)), 4)
        self.assertEqual(e2e.managed_resources({}), [])


class ImportPlanTests(unittest.TestCase):
    def test_pure_adoption_is_accepted(self):
        plan = {"resource_changes": [
            change("obot_mcp_tunnel.e2e", "obot_mcp_tunnel", ["no-op"]),
            change("obot_git_credential.private", "obot_git_credential", ["update"],
                   {"token_wo_version": None}, {"token_wo_version": 1}),
            {"address": "data.obot_mcp_tunnel.e2e", "mode": "data", "change": {"actions": ["read"]}},
        ]}
        self.assertEqual(e2e.import_problems(plan, ["obot_mcp_tunnel.e2e", "obot_git_credential.private"]), [])

    def test_attribute_drift_after_import_is_reported(self):
        plan = {"resource_changes": [change("obot_mcp_tunnel.e2e", "obot_mcp_tunnel", ["update"],
                                            {"description": "a"}, {"description": "b"})]}
        self.assertEqual(e2e.import_problems(plan, ["obot_mcp_tunnel.e2e"]),
                         ["obot_mcp_tunnel.e2e plans update of description"])

    def test_git_credential_may_only_resend_its_token(self):
        plan = {"resource_changes": [change("obot_git_credential.private", "obot_git_credential", ["update"],
                                            {"host": "a", "token_wo_version": None},
                                            {"host": "b", "token_wo_version": 1})]}
        self.assertEqual(e2e.import_problems(plan, ["obot_git_credential.private"]),
                         ["obot_git_credential.private plans update of host, token_wo_version"])

    def test_create_replace_or_missing_import_is_reported(self):
        plan = {"resource_changes": [
            change("obot_mcp_tunnel.e2e", "obot_mcp_tunnel", ["create"], importing=False),
            change("obot_mcp_access_rule.e2e", "obot_mcp_access_rule", ["delete", "create"]),
        ]}
        problems = e2e.import_problems(plan, ["obot_mcp_tunnel.e2e", "obot_mcp_access_rule.e2e",
                                              "obot_git_credential.private"])
        self.assertIn("obot_mcp_tunnel.e2e was not imported", problems)
        self.assertIn("obot_mcp_access_rule.e2e plans delete/create of nothing", problems)
        self.assertIn("obot_git_credential.private is missing from the import plan", problems)


class ProvenanceTests(unittest.TestCase):
    def test_image_revision_from_obot_version(self):
        self.assertEqual(obot.image_revision("v0.0.0-dev+2abe6df3"), "2abe6df3")
        self.assertEqual(obot.image_revision("v0.27.0+a6070110"), "a6070110")
        self.assertIsNone(obot.image_revision("v0.27.0"))
        self.assertIsNone(obot.image_revision(""))


if __name__ == "__main__":
    unittest.main()
