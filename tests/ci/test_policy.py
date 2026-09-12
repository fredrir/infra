import json
import re
import sys
import tempfile
import unittest
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/ci"))
import policy


class ExecutionPolicyTests(unittest.TestCase):
    def setUp(self):
        self.context = {
            "repository_owner_id": "114402558",
            "repository_id": "773018612",
            "event_name": "push",
            "ref": "refs/heads/main",
            "ref_protected": True,
        }

    def test_protected_approved_repository_can_build(self):
        self.assertTrue(policy.authorize(self.context))

    def test_external_events_never_authorize_even_with_main_ref(self):
        for event in (
            "pull_request",
            "pull_request_target",
            "issue_comment",
            "workflow_run",
            "workflow_dispatch",
            "repository_dispatch",
        ):
            with self.subTest(event=event):
                self.assertFalse(
                    policy.authorize({**self.context, "event_name": event})
                )

    def test_owner_name_cannot_replace_verified_identity(self):
        self.assertFalse(
            policy.authorize(
                {
                    **self.context,
                    "repository_owner": "fredrir",
                    "repository_owner_id": "7",
                }
            )
        )

    def test_fork_with_matching_repository_name_is_rejected(self):
        self.assertFalse(
            policy.authorize(
                {
                    **self.context,
                    "repository": "fredrir/portfolio",
                    "repository_id": "7",
                }
            )
        )

    def test_tags_and_unprotected_branches_are_rejected(self):
        self.assertFalse(policy.authorize({**self.context, "ref": "refs/tags/main"}))
        self.assertFalse(policy.authorize({**self.context, "ref_protected": False}))

    def test_application_cannot_promote_infrastructure(self):
        self.assertFalse(policy.authorize(self.context, infrastructure=True))

    def test_staged_native_architectures_require_control_capacity(self):
        self.assertEqual(policy.available_architectures('["amd64"]'), ["amd64"])
        for value in ("[]", '["arm64"]', '["amd64","amd64"]', '["amd64","s390x"]'):
            with self.subTest(value=value), self.assertRaises(policy.PolicyError):
                policy.available_architectures(value)


class ProjectContractTests(unittest.TestCase):
    def setUp(self):
        self.catalog = {
            "projects": {
                "portfolio": {
                    "repositoryId": 773018612,
                    "architectures": ["amd64", "arm64"],
                }
            }
        }

    def test_project_identity_and_native_platform_are_required(self):
        result = policy.resolve(self.catalog, "portfolio", 773018612, ["linux/arm64"])
        self.assertEqual(result["repositoryId"], 773018612)
        with self.assertRaises(policy.PolicyError):
            policy.resolve(self.catalog, "portfolio", 900286164, ["linux/arm64"])

    def test_duplicate_or_unsupported_platforms_are_rejected(self):
        for platforms in (
            [],
            ["linux/amd64", "linux/amd64"],
            ["windows/amd64"],
            ["linux/s390x"],
        ):
            with (
                self.subTest(platforms=platforms),
                self.assertRaises(policy.PolicyError),
            ):
                policy.resolve(self.catalog, "portfolio", 773018612, platforms)

    def test_build_paths_cannot_escape_through_parent_or_symlink(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "source"
            root.mkdir()
            (root / "escape").symlink_to(Path(directory))
            for path in ("../outside", "/etc/passwd", "escape/file"):
                with self.subTest(path=path), self.assertRaises(policy.PolicyError):
                    policy.relative_path(root, path)

    def test_component_image_is_owned_by_the_catalog(self):
        entry = {
            "image": "ghcr.io/fredrir/portfolio",
            "images": {"web": "ghcr.io/fredrir/portfolio-web"},
        }
        self.assertEqual(
            policy.component_image(entry, "web"), "ghcr.io/fredrir/portfolio-web"
        )
        for component in ("app", "api", "../web", "web\n", "A"):
            with (
                self.subTest(component=component),
                self.assertRaises(policy.PolicyError),
            ):
                policy.component_image(entry, component)

    def test_legacy_app_and_named_component_use_separate_release_paths(self):
        self.assertEqual(
            policy.release_path("portfolio"), "platform/projects/portfolio/release.json"
        )
        self.assertEqual(
            policy.release_path("portfolio", "web"),
            "platform/projects/portfolio/releases/web.json",
        )
        with self.assertRaises(policy.PolicyError):
            policy.release_path("../portfolio", "web")


class WorkflowBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        (self.root / ".github/workflows").mkdir(parents=True)
        self.workflow = {
            "on": {"push": {"branches": ["main"]}},
            "jobs": {
                "check": {
                    "if": policy.guard(True),
                    "runs-on": "${{ format('infra-{0}-amd64', github.repository_id) }}",
                    "container": {"image": "${{ vars.INFRA_CI_IMAGE }}"},
                    "steps": [{"uses": "actions/checkout@" + "a" * 40}],
                }
            },
        }

    def validate(self):
        (self.root / ".github/workflows/check.yml").write_text(
            yaml.safe_dump(self.workflow)
        )
        policy.validate_workflows(self.root)

    def test_guarded_self_hosted_workflow_passes(self):
        self.validate()

    def test_added_pr_trigger_is_rejected(self):
        self.workflow["on"]["pull_request"] = None
        with self.assertRaisesRegex(policy.PolicyError, "forbidden workflow trigger"):
            self.validate()

    def test_yaml_extension_cannot_bypass_workflow_audit(self):
        forbidden = {"on": {"pull_request": None}, "jobs": {}}
        (self.root / ".github/workflows/other.yaml").write_text(
            yaml.safe_dump(forbidden)
        )
        with self.assertRaisesRegex(policy.PolicyError, "forbidden workflow trigger"):
            self.validate()

    def test_runner_hosted_fallback_is_rejected(self):
        self.workflow["jobs"]["check"]["runs-on"] = "ubuntu-latest"
        with self.assertRaisesRegex(policy.PolicyError, "forbidden runner"):
            self.validate()

    def test_step_guard_cannot_replace_job_guard(self):
        job = self.workflow["jobs"]["check"]
        job.pop("if")
        job["steps"][0]["if"] = policy.guard(True)
        with self.assertRaisesRegex(policy.PolicyError, "pre-allocation"):
            self.validate()

    def test_mutable_action_tag_is_rejected(self):
        self.workflow["jobs"]["check"]["steps"][0]["uses"] = "actions/checkout@main"
        with self.assertRaisesRegex(policy.PolicyError, "not pinned"):
            self.validate()

    def test_nested_unapproved_workflow_cannot_escape_runner_policy(self):
        self.workflow["jobs"]["check"].pop("runs-on")
        self.workflow["jobs"]["check"]["uses"] = (
            "other/workflows/.github/workflows/ci.yml@" + "a" * 40
        )
        with self.assertRaisesRegex(policy.PolicyError, "unapproved reusable workflow"):
            self.validate()

    def test_repository_workflows_obey_execution_policy(self):
        policy.validate_workflows(ROOT)


class AuthenticationPlanTests(unittest.TestCase):
    def test_sts_policy_binds_repository_and_exact_shared_revision(self):
        result = policy.sts_policy({"repositoryId": 773018612}, "a" * 40)
        claims = result["claim_pattern"]
        self.assertIsNotNone(re.fullmatch(claims["repository_id"], "773018612"))
        self.assertIsNone(re.fullmatch(claims["repository_id"], "900286164"))
        self.assertIsNone(re.fullmatch(claims["event_name"], "pull_request_target"))
        self.assertIsNone(re.fullmatch(claims["job_workflow_sha"], "b" * 40))
        self.assertEqual(
            result["permissions"], {"contents": "write", "pull_requests": "write"}
        )

    def test_sts_policy_rejects_mutable_workflow_ref(self):
        with self.assertRaises(policy.PolicyError):
            policy.sts_policy({"repositoryId": 773018612}, "main")

    def test_settings_plan_requires_all_external_contributors(self):
        plan = policy.settings_plan("fredrir/portfolio")
        self.assertEqual(
            plan[0]["body"], {"approval_policy": "all_external_contributors"}
        )
        self.assertFalse(plan[1]["body"]["can_approve_pull_request_reviews"])

    def test_settings_plan_rejects_other_owner(self):
        with self.assertRaises(policy.PolicyError):
            policy.settings_plan("other/portfolio")

    def test_checked_in_sts_policies_are_disabled_or_exactly_scoped(self):
        for path in (ROOT / ".github/chainguard").glob("*.sts.yaml"):
            definition = yaml.safe_load(path.read_text())
            revision = definition["claim_pattern"]["job_workflow_sha"]
            if revision == "^disabled$":
                self.assertEqual(
                    definition["claim_pattern"]["job_workflow_ref"], "^disabled$"
                )
            else:
                self.assertRegex(revision, r"^\^[0-9a-f]{40}\$$")
                project = path.name.removesuffix("-release.sts.yaml")
                self.assertEqual(
                    definition,
                    policy.sts_policy(
                        policy.load_catalog()["projects"][project], revision[1:-1]
                    ),
                )


class PublicBuildArgumentTests(unittest.TestCase):
    def setUp(self):
        self.entry = policy.load_catalog()["projects"]["portfolio"]

    def test_public_browser_key_is_scoped_to_its_component(self):
        value = '{"VITE_POSTHOG_KEY":"public-browser-key"}'
        self.assertEqual(
            policy.public_build_arguments(self.entry, "web", value),
            {"VITE_POSTHOG_KEY": "public-browser-key"},
        )
        with self.assertRaises(policy.PolicyError):
            policy.public_build_arguments(self.entry, "api", value)

    def test_credentials_are_rejected_even_if_catalog_mislabels_them(self):
        for name in [
            "ADMIN_TOKEN",
            "AWS_ACCESS_KEY_ID",
            "DB_PASSWORD",
            "DOPPLER_TOKEN",
            "PRIVATE_KEY",
            "AUTH_HEADER",
        ]:
            entry = {**self.entry, "publicBuildArguments": {"web": [name]}}
            with self.subTest(name=name), self.assertRaises(policy.PolicyError):
                policy.public_build_arguments(
                    entry, "web", json.dumps({name: "fixture"})
                )

    def test_ambiguous_or_non_string_arguments_are_rejected(self):
        values = [
            "[]",
            '{"VITE_APP_VERSION":1}',
            '{"VITE_APP_VERSION":null}',
            '{"VITE_APP_VERSION":"a","VITE_APP_VERSION":"b"}',
            json.dumps({"VITE_APP_VERSION": "a\nb"}),
            json.dumps({"VITE_APP_VERSION": "a" * 2049}),
        ]
        for value in values:
            with self.subTest(value=value[:80]), self.assertRaises(policy.PolicyError):
                policy.public_build_arguments(self.entry, "web", value)

    def test_configuration_hash_is_canonical_and_binds_all_build_inputs(self):
        inputs = [
            ".",
            "apps/web/Dockerfile",
            ".#web",
            "npm test",
            '{"VITE_APP_VERSION":"v1","VITE_POSTHOG_KEY":"public-key"}',
        ]
        _, expected = policy.build_configuration(self.entry, "web", *inputs)
        reordered = '{ "VITE_POSTHOG_KEY": "public-key", "VITE_APP_VERSION": "v1" }'
        self.assertEqual(
            expected,
            policy.build_configuration(self.entry, "web", *inputs[:-1], reordered)[1],
        )
        for index, value in enumerate(
            [
                "apps/web",
                "Dockerfile",
                ".#other",
                "npm run test:integration",
                '{"VITE_APP_VERSION":"v2"}',
            ]
        ):
            changed = inputs.copy()
            changed[index] = value
            with self.subTest(index=index):
                self.assertNotEqual(
                    expected, policy.build_configuration(self.entry, "web", *changed)[1]
                )
