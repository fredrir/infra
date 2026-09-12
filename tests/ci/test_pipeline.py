import base64
import json
import sys
import tempfile
import unittest
import urllib.error
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/ci"))
import pipeline
from policy import PolicyError


class ArtifactTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.context = {
            "repositoryId": 773018612,
            "sourceRevision": "a" * 40,
            "workflowRevision": "b" * 40,
        }

    def artifact(self, architecture, **changes):
        directory = self.root / f"build-{architecture}"
        directory.mkdir()
        archive = directory / "image.tar"
        archive.write_bytes(b"artifact fixture")
        manifest = {
            **self.context,
            "schemaVersion": 1,
            "platform": f"linux/{architecture}",
            "archiveSha256": pipeline.sha256(archive),
            **changes,
        }
        (directory / "build.json").write_text(json.dumps(manifest))
        return archive

    def test_all_requested_native_artifacts_are_required(self):
        self.artifact("amd64")
        with self.assertRaisesRegex(PolicyError, "incomplete"):
            pipeline.validate_artifacts(
                self.root, ["linux/amd64", "linux/arm64"], self.context
            )
        self.artifact("arm64")
        self.assertEqual(
            len(
                pipeline.validate_artifacts(
                    self.root, ["linux/amd64", "linux/arm64"], self.context
                )
            ),
            2,
        )

    def test_changed_artifact_bytes_are_rejected(self):
        archive = self.artifact("amd64")
        archive.write_bytes(b"modified")
        with self.assertRaisesRegex(PolicyError, "digest mismatch"):
            pipeline.validate_artifacts(self.root, ["linux/amd64"], self.context)

    def test_artifacts_from_another_commit_are_rejected(self):
        self.artifact("amd64", sourceRevision="c" * 40)
        with self.assertRaisesRegex(PolicyError, "sourceRevision mismatch"):
            pipeline.validate_artifacts(self.root, ["linux/amd64"], self.context)

    def test_wrong_repository_artifacts_are_rejected(self):
        self.artifact("amd64", repositoryId=900286164)
        with self.assertRaisesRegex(PolicyError, "repositoryId mismatch"):
            pipeline.validate_artifacts(self.root, ["linux/amd64"], self.context)

    def test_sibling_component_artifact_is_rejected(self):
        self.artifact("amd64", component="api")
        with self.assertRaisesRegex(PolicyError, "component mismatch"):
            pipeline.validate_artifacts(
                self.root, ["linux/amd64"], {**self.context, "component": "web"}
            )

    def test_artifact_from_a_different_run_is_rejected(self):
        self.artifact("amd64", runId=123)
        with self.assertRaisesRegex(PolicyError, "runId mismatch"):
            pipeline.validate_artifacts(
                self.root, ["linux/amd64"], {**self.context, "runId": 456}
            )


class ProvenanceTests(unittest.TestCase):
    def setUp(self):
        self.context = {
            "repositoryId": 773018612,
            "repository": "fredrir/portfolio",
            "sourceRevision": "a" * 40,
            "workflowRevision": "b" * 40,
            "workflowRepository": "fredrir/llunde-infra",
            "runId": 123,
        }
        self.platforms = ["linux/amd64", "linux/arm64"]
        self.image = "ghcr.io/fredrir/portfolio@sha256:" + "c" * 64
        self.statement = {
            "_type": "https://in-toto.io/Statement/v1",
            "subject": [
                {"name": "ghcr.io/fredrir/portfolio", "digest": {"sha256": "c" * 64}}
            ],
            "predicateType": pipeline.PROVENANCE_TYPE,
            "predicate": pipeline.predicate(self.context, self.platforms),
        }

    def record(self, statement=None):
        return {
            "payload": base64.b64encode(
                json.dumps(statement or self.statement).encode()
            ).decode()
        }

    def test_matching_verified_statement_is_accepted(self):
        pipeline.validate_attestations(
            [self.record()], self.image, self.context, self.platforms
        )

    def test_consumer_cannot_substitute_a_mutable_shared_workflow_ref(self):
        with self.assertRaises(PolicyError):
            pipeline.validate_workflow_identity(
                {
                    **self.context,
                    "workflowRef": "fredrir/llunde-infra/.github/workflows/project-ci.yml@refs/heads/main",
                }
            )

    def test_infrastructure_local_workflow_ref_requires_matching_source_commit(self):
        ctx = {
            **self.context,
            "repositoryId": 1328085692,
            "repository": "fredrir/llunde-infra",
            "sourceRevision": "b" * 40,
            "workflowRef": "fredrir/llunde-infra/.github/workflows/project-ci.yml@refs/heads/main",
        }
        pipeline.validate_workflow_identity(ctx)
        with self.assertRaises(PolicyError):
            pipeline.validate_workflow_identity({**ctx, "sourceRevision": "a" * 40})

    def test_verified_attestation_for_other_image_is_rejected(self):
        self.statement["subject"][0]["digest"]["sha256"] = "d" * 64
        with self.assertRaises(PolicyError):
            pipeline.validate_attestations(
                [self.record()], self.image, self.context, self.platforms
            )

    def test_verified_but_wrong_source_workflow_or_run_is_rejected(self):
        for key, value in (
            ("sourceRevision", "d" * 40),
            ("workflowRevision", "d" * 40),
            ("repositoryId", 900286164),
            ("runId", 456),
        ):
            with self.subTest(key=key), self.assertRaises(PolicyError):
                pipeline.validate_attestations(
                    [self.record()],
                    self.image,
                    {**self.context, key: value},
                    self.platforms,
                )

    def test_untested_extra_platform_is_rejected(self):
        with self.assertRaises(PolicyError):
            pipeline.validate_attestations(
                [self.record()], self.image, self.context, ["linux/amd64"]
            )

    def test_sibling_component_attestation_is_rejected(self):
        with self.assertRaises(PolicyError):
            pipeline.validate_attestations(
                [self.record()],
                self.image,
                {**self.context, "component": "web"},
                self.platforms,
            )

    def test_json_stream_and_array_outputs_are_supported(self):
        record = self.record()
        self.assertEqual(
            pipeline.parse_records(json.dumps(record) + "\n" + json.dumps(record)),
            [record, record],
        )
        self.assertEqual(pipeline.parse_records(json.dumps([record])), [record])


class ReleaseBranchTests(unittest.TestCase):
    def test_conflicting_component_update_rebases_and_never_forces(self):
        references = iter(["first", "sibling"])
        trees = []
        updates = []

        def api(method, path, body=None):
            if method == "GET" and "/git/ref/" in path:
                return {"object": {"sha": next(references)}}
            if method == "GET" and "/git/commits/" in path:
                return {"tree": {"sha": path.rsplit("/", 1)[1] + "-tree"}}
            if path.endswith("/git/trees"):
                trees.append(body)
                return {"sha": "new-tree"}
            if path.endswith("/git/commits"):
                return {"sha": "new-commit"}
            if method == "PATCH":
                updates.append(body)
                if len(updates) == 1:
                    raise urllib.error.HTTPError(path, 422, "conflict", None, None)
                return {}
            self.fail(f"Unexpected request: {method} {path}")

        document = {
            "project": "portfolio",
            "component": "web",
            "sourceRevision": "a" * 40,
        }
        with patch.object(pipeline, "api", side_effect=api):
            pipeline.update_release_branch(
                "fredrir/llunde-infra", "release/portfolio/run", document
            )
        self.assertEqual(
            [tree["base_tree"] for tree in trees], ["first-tree", "sibling-tree"]
        )
        self.assertTrue(all(update["force"] is False for update in updates))
        self.assertEqual(
            trees[-1]["tree"][0]["path"],
            "platform/projects/portfolio/releases/web.json",
        )
        self.assertEqual(len(trees[-1]["tree"]), 1)


class BuildConfigurationTests(unittest.TestCase):
    def test_public_values_are_literal_subprocess_arguments(self):
        value = "v1 $(touch /tmp/must-not-run); --secret id=credential"
        command = pipeline.buildkit_arguments(
            "unix:///tmp/socket",
            Path("/source"),
            Path("/source/Dockerfile"),
            "linux/amd64",
            Path("/artifact"),
            {"APP_VERSION": value},
        )
        self.assertEqual(command[-2:], ["--opt", "build-arg:APP_VERSION=" + value])
        self.assertNotIn("--secret", command)
        self.assertNotIn("sh", command)

    def test_artifact_cannot_substitute_different_public_configuration(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            artifact = root / "build-amd64"
            artifact.mkdir()
            (artifact / "image.tar").write_bytes(b"fixture")
            context = {
                "repositoryId": 773018612,
                "sourceRevision": "a" * 40,
                "workflowRevision": "b" * 40,
                "buildConfigSha256": "c" * 64,
            }
            manifest = {
                **context,
                "schemaVersion": 1,
                "platform": "linux/amd64",
                "archiveSha256": pipeline.sha256(artifact / "image.tar"),
                "buildConfigSha256": "d" * 64,
            }
            (artifact / "build.json").write_text(json.dumps(manifest))
            with self.assertRaisesRegex(PolicyError, "buildConfigSha256 mismatch"):
                pipeline.validate_artifacts(root, ["linux/amd64"], context)

    def test_verified_image_attestation_binds_configuration_hash(self):
        context = {
            "repositoryId": 773018612,
            "repository": "fredrir/portfolio",
            "sourceRevision": "a" * 40,
            "workflowRevision": "b" * 40,
            "workflowRepository": "fredrir/llunde-infra",
            "runId": 123,
            "buildConfigSha256": "c" * 64,
        }
        platforms = ["linux/amd64"]
        statement = {
            "subject": [{"digest": {"sha256": "d" * 64}}],
            "predicateType": pipeline.PROVENANCE_TYPE,
            "predicate": pipeline.predicate(context, platforms),
        }
        record = {"payload": base64.b64encode(json.dumps(statement).encode()).decode()}
        pipeline.validate_attestations(
            [record], "ghcr.io/fredrir/portfolio@sha256:" + "d" * 64, context, platforms
        )
        with self.assertRaises(PolicyError):
            pipeline.validate_attestations(
                [record],
                "ghcr.io/fredrir/portfolio@sha256:" + "d" * 64,
                {**context, "buildConfigSha256": "e" * 64},
                platforms,
            )
