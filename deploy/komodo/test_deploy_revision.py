"""Tests for the pure parts of deploy-revision.py."""
import importlib.util
import unittest
from pathlib import Path


def load():
    path = Path(__file__).with_name("deploy-revision.py")
    spec = importlib.util.spec_from_file_location("deploy_revision", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class RewriteRevisionTest(unittest.TestCase):
    def setUp(self):
        self.mod = load()

    def test_replaces_the_one_line_and_keeps_the_rest(self):
        env = "HUB_PORT=8080\nHUB_BUILD_REVISION=0000000\nDATABASE_URL=[[HUB_DATABASE_URL]]\n"
        got = self.mod.rewrite_revision(env, "abc1234")
        self.assertEqual(
            got,
            "HUB_PORT=8080\nHUB_BUILD_REVISION=abc1234\nDATABASE_URL=[[HUB_DATABASE_URL]]\n",
        )

    def test_rejects_a_missing_or_duplicate_line(self):
        with self.assertRaises(ValueError):
            self.mod.rewrite_revision("HUB_PORT=8080\n", "abc1234")
        with self.assertRaises(ValueError):
            self.mod.rewrite_revision(
                "HUB_BUILD_REVISION=a\nHUB_BUILD_REVISION=b\n", "abc1234"
            )

    def test_rejects_a_revision_that_is_not_hex(self):
        with self.assertRaises(ValueError):
            self.mod.rewrite_revision("HUB_BUILD_REVISION=0000000\n", "ABC1234")
        with self.assertRaises(ValueError):
            self.mod.rewrite_revision("HUB_BUILD_REVISION=0000000\n", "short")


class RedactTest(unittest.TestCase):
    def test_hides_secret_values_and_keeps_the_revision_line(self):
        mod = load()
        text = "\n".join([
            "HUB_BUILD_REVISION=abc1234",
            "DATABASE_URL=postgres://postgres:replace-me@postgres:5432/git-event-hub",
            "GITHUB_WEBHOOK_SECRET=replace-me",
            "hub container failed to start",
        ])
        got = mod.redact(text)
        self.assertIn("HUB_BUILD_REVISION=abc1234", got)
        self.assertIn("DATABASE_URL=<redacted>", got)
        self.assertIn("GITHUB_WEBHOOK_SECRET=<redacted>", got)
        self.assertIn("hub container failed to start", got)
        self.assertNotIn("replace-me", got)


class TagCheckTest(unittest.TestCase):
    def setUp(self):
        self.mod = load()

    def test_accepts_only_the_hub_image_with_the_exact_tag(self):
        image = "ghcr.io/kevinle128/git-event-hub:abc1234"
        self.assertTrue(self.mod.image_has_revision(image, "abc1234"))
        self.assertFalse(self.mod.image_has_revision(image, "abc123"))
        self.assertFalse(self.mod.image_has_revision(image, "bc1234"))
        self.assertFalse(self.mod.image_has_revision("postgres:16-alpine", "16-alpine"))
        self.assertFalse(self.mod.image_has_revision("postgres:16-alpine", "abc1234"))

    def test_hub_ready_needs_one_healthy_container_on_that_tag(self):
        good = [{"Image": "ghcr.io/kevinle128/git-event-hub:abc1234", "Health": "healthy"}]
        self.assertTrue(self.mod.hub_ready(good, "abc1234"))
        self.assertFalse(self.mod.hub_ready([], "abc1234"))
        starting = [{"Image": "ghcr.io/kevinle128/git-event-hub:abc1234", "Health": "starting"}]
        self.assertFalse(self.mod.hub_ready(starting, "abc1234"))
        old = [{"Image": "ghcr.io/kevinle128/git-event-hub:0000000", "Health": "healthy"}]
        self.assertFalse(self.mod.hub_ready(old, "abc1234"))


if __name__ == "__main__":
    unittest.main()
