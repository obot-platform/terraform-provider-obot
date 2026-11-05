"""Behavior checks for Obot version resolution; no network is required."""
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import obot_versions as versions

TAGS = ["v0.25.6", "v0.26.0", "v0.26.0-rc1", "v0.26.2", "v0.26.3", "v0.27.0-rc2",
        "v0.27.0", "v0.28.0-alpha3", "not-a-version"]


class ResolveTests(unittest.TestCase):
    def test_series_resolve_to_newest_stable_patch_and_main_is_always_tested(self):
        plan = versions.resolve(["v0.26", "v0.27"], TAGS)
        self.assertEqual(plan["refs"], ["v0.26.3", "v0.27.0", "main"])
        self.assertEqual(plan["warnings"], [])

    def test_prereleases_are_never_selected(self):
        plan = versions.resolve(["v0.27"], TAGS + ["v0.27.1-rc1"], discover=True)
        self.assertEqual(plan["refs"], ["v0.27.0", "main"])
        self.assertEqual(plan["newest_release"], "v0.27.0")

    def test_upgrades_step_through_each_release_then_to_main(self):
        plan = versions.resolve(["v0.26", "v0.27"], TAGS)
        self.assertEqual(plan["upgrades"], [{"from": "v0.26.3", "to": "v0.27.0"},
                                            {"from": "v0.27.0", "to": "main"}])

    def test_discovery_tests_an_unlisted_newer_release_and_warns(self):
        plan = versions.resolve(["v0.26", "v0.27"], TAGS + ["v0.28.0", "v0.28.1"], discover=True)
        self.assertEqual(plan["refs"], ["v0.26.3", "v0.27.0", "v0.28.1", "main"])
        self.assertFalse(plan["releases"][-1]["listed"])
        self.assertEqual(len(plan["warnings"]), 1)
        self.assertIn("v0.28", plan["warnings"][0])
        self.assertIn({"from": "v0.28.1", "to": "main"}, plan["upgrades"])

    def test_without_discovery_only_listed_versions_are_tested(self):
        plan = versions.resolve(["v0.26", "v0.27"], TAGS + ["v0.28.0"])
        self.assertEqual(plan["refs"], ["v0.26.3", "v0.27.0", "main"])
        self.assertEqual(plan["warnings"], [])

    def test_exact_pin_is_honored_and_flagged_when_a_patch_exists(self):
        plan = versions.resolve(["v0.26.2", "v0.27"], TAGS)
        self.assertEqual(plan["refs"], ["v0.26.2", "v0.27.0", "main"])
        self.assertEqual(len(plan["warnings"]), 1)
        self.assertIn("v0.26.3", plan["warnings"][0])

    def test_config_order_does_not_change_test_order(self):
        plan = versions.resolve(["v0.27", "v0.26"], TAGS)
        self.assertEqual(plan["refs"], ["v0.26.3", "v0.27.0", "main"])

    def test_unknown_series_or_tag_is_an_error(self):
        with self.assertRaisesRegex(versions.ConfigError, "v0.99"):
            versions.resolve(["v0.99"], TAGS)
        with self.assertRaisesRegex(versions.ConfigError, "v0.26.9"):
            versions.resolve(["v0.26.9"], TAGS)


class ConfigTests(unittest.TestCase):
    def config(self, content):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        path = Path(directory.name) / "obot-versions.json"
        path.write_text(json.dumps(content))
        return path

    def test_repository_config_is_valid(self):
        entries = versions.load_config()
        self.assertTrue(entries)

    def test_invalid_entries_are_rejected(self):
        for content in [{}, {"supported": []}, {"supported": ["main"]}, {"supported": ["0.27"]},
                        {"supported": ["v0.27", "v0.27"]}, {"supported": ["v0.27.0-rc1"]}]:
            with self.subTest(content=content), self.assertRaises(versions.ConfigError):
                versions.load_config(self.config(content))

    def test_newest_stable_provider_tag(self):
        self.assertEqual(versions.newest_stable(["v0.1.0", "v0.10.0", "v0.2.0", "v1.0.0-rc1"]), "v0.10.0")
        self.assertIsNone(versions.newest_stable([]))

    def test_github_output_lists_matrix_values(self):
        with tempfile.NamedTemporaryFile("r") as output, \
                patch.dict(versions.os.environ, {"GITHUB_OUTPUT": output.name}), \
                patch("builtins.print"):
            versions.write_github_output(versions.resolve(["v0.27"], TAGS), "v0.1.0")
            values = dict(line.split("=", 1) for line in output.read().splitlines())
        self.assertEqual(json.loads(values["refs"]), ["v0.27.0", "main"])
        self.assertEqual(values["provider_release"], "v0.1.0")
        self.assertEqual(json.loads(values["upgrades"]), [{"from": "v0.27.0", "to": "main"}])


if __name__ == "__main__":
    unittest.main()
