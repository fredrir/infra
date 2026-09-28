# Infrastructure rollout

| Stage | Requirement | Action |
| --- | --- | --- |
| Source | Reviewed, owner-signed changes; relevant local checks pass | Push to `main` |
| Reconciliation | Hosted checks and images pass; provenance and apply gates pass | `fredrir-11` reconciles `main` and publishes `production`; [runbook](runbook.md#reconciler-host) |
| CLI release | Source belongs to `main`; protected, signed `infra-v*` tag | Publish four platform binaries through `cli-release.yml` |
| Binary trust | Hosted provenance binds the asset to the expected workflow, source and tag; SHA-256 matches | Verify before installation; retain a verified recovery copy until qualification completes |
| CLI pin | Verified tag, revision, Linux amd64 SHA-256 and matching input digest | Update [`build/cli-release.json`](../build/cli-release.json); use the [input producer](../.github/actions/cli-inputs/action.yml) |
| Build VM | Per-repository accounts, engines and caches | Normal reconciliation installs the CLI and converges runner services |
| Reconciler CLI | Reviewed release pin | Administrator applies only `infra_binary` on `fredrir-11` |
| Images | Published digest, vulnerability checks and provenance | Promote immutable image pins and verify ready workloads |
| External callers | Exact reviewed workflow SHA and matching OctoSTS claims | Update each caller and trust policy together; verify its actual build and deployment |

## CLI installation

The release manifest and checksums must be authenticated before these commands. Read the revision and Linux amd64 digest from the verified pin; a checksum beside an unauthenticated binary is insufficient.

```sh
infra artifact install \
  --url "$INFRA_BINARY_URL" \
  --revision "$INFRA_REVISION" \
  --sha256 "$INFRA_SHA256" \
  --platform linux/amd64 \
  --destination "$HOME/.local/bin/infra"

(cd ansible && ansible-playbook reconciler.yml --limit fredrir-11 --tags infra_binary)
```

The reconciler playbook is an administrator operation; ordinary reconciliation does not install that role. The binary role removes superseded cache revisions. Pause the timers during an administrative replacement, let active services finish, then restore their prior state and verify a subsequent apply and full verification.

## Image promotion

```sh
infra platform promote-tools --image "$VERIFIED_TOOLS_IMAGE"
infra platform promote-tools --image "$VERIFIED_TOOLS_IMAGE" --apply
git diff -- platform
```

`VERIFIED_TOOLS_IMAGE` must be a published `ghcr.io/fredrir/platform-backup-tools@sha256:...` reference. For private images, use the dedicated registry reader for preflight checks; validate application files and entry points under the production UID before deployment.

## Qualification records

| Check | Evidence |
| --- | --- |
| Cache access, protected release, ordinary checks and uncached limits | [Bazel cache qualification](../build/evidence/bazel-cache-ci.json) |
| Build VM isolation | [Repository isolation](../build/evidence/build-vm-repository-isolation.json) |
| Application file permissions, images and parser publication | [Consumer rollout](../build/evidence/consumer-source-mode-rollout.json) |
| Flux ownership and measured deployment latency | [Flux qualification](../build/evidence/flux-artifact-qualification.json) |

The receipts distinguish successful ordinary checks from forced uncached timings, and observed rollout latency from target latency. Native ARM execution, full production restore and Kata runtime activation require their own qualification.
