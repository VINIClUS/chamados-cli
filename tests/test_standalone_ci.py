"""Static checks for the standalone GitHub Actions workflows."""

from pathlib import Path
import unittest


ROOT = Path(__file__).resolve().parents[1]


class StandaloneCI(unittest.TestCase):
    def test_ci_runs_contracts_tests_and_six_platform_build(self):
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        self.assertIn("go test -race ./...", workflow)
        self.assertIn("go vet ./...", workflow)
        self.assertIn("scripts/cli/check-contracts.py", workflow)
        self.assertIn("scripts/cli/build.sh", workflow)

    def test_release_is_tagged_and_verifies_checksums(self):
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        self.assertIn('"cli-v*.*.*"', workflow)
        self.assertIn("sha256sum --check SHA256SUMS", workflow)
        self.assertIn("gh release upload", workflow)


if __name__ == "__main__":
    unittest.main()
