# Shared platform

| Name | Value |
| --- | --- |
| Desired configuration | [Inventory](../platform/inventory/nodes.json), [versions](../platform/versions.yaml), [project catalog](../platform/catalog/projects.json) |
| Runtime | K3s; three Hetzner control planes; shared production and CI workers |
| Hosts | NixOS 26.05; Ubuntu 24.04/26.04 or Debian 13 through Ansible |
| Scheduling | Kubernetes affinity, protected capability labels, priorities and quotas |
| Activation | Suspended Flux layers, zero ARC capacity, disabled cache and backup jobs |
| Existing applications | Legacy host definitions remain active until individual cutovers |
| ARM capacity | HidenCloud SAR-Torrent remains unpurchased and unverified |
| Repository visibility | Private until publication review is complete |

## Ownership

| Owner | Resources |
| --- | --- |
| OpenTofu | Provider machines, private networks, DNS records and import mappings |
| Host adapter | SSH, firewall, Tailscale, kernel, K3s, containerd, local disks and etcd recovery |
| Flux platform reconciler | Cluster policy, shared services, project namespaces, baseline isolation and secret stores |
| Namespace Helm reconcilers | Separate application and data releases; approved workloads, services and ingress |
| Project repository | Source, tests, Dockerfiles and a caller pinned to the shared workflow commit |
| Project catalog | Numeric repository identity, images, domains, architectures and capabilities |
| Release records | Verified image digest, source revision, workflow revision and workflow run |

Provider integrations are optional adapters; an existing SSH-accessible machine does not require a cloud API. Existing OpenTofu addresses and backend remain unchanged. Import provisioned resources with verified IDs before enabling their declarations; review the saved plan before applying.

## Local validation

```sh
uv sync --frozen --group ci
uv run --frozen infra validate
uv run --frozen infra render --check
uv run --frozen --group ci python scripts/ci/policy.py check
uv run --frozen --group ci python scripts/operations/platform.py check
uv run --frozen --group ci python -m unittest discover -s tests/infra -v
uv run --frozen --group ci python -m unittest discover -s tests/ci -v
uv run --frozen --group ci python -m unittest discover -s tests/operations -v
uv run --frozen --group ci python -m unittest discover -s ansible/tests -v
uv run --frozen --group ci ansible-playbook -i ansible/inventory/fleet.py ansible/site.yml --syntax-check
helm lint charts/project -f tests/infra/fixtures/values.yaml
```

These checks validate the implementation; they do not establish host compatibility, network reachability, runtime isolation or successful recovery.

## Project onboarding

Use a published, reviewed infrastructure commit for `workflow_revision`.

```sh
uv run --frozen infra onboard fredrir/portfolio \
  --domain hansteen.dev \
  --port 3000 \
  --health-path / \
  --architecture amd64 \
  --test-command 'npm test' \
  --workflow-ref "$workflow_revision" \
  --output .infra/onboarding/portfolio
```

| Generated file | Destination |
| --- | --- |
| `project.yaml` | Reviewed `platform/projects/<project>/project.yaml` in this repository |
| `.github/workflows/ci.yml` | Project repository; replace the example test command with its actual test entrypoint |
| `onboarding-request.json` | Catalog review; new repositories receive no authority automatically |

The command validates GitHub's numeric repository and owner identities. Public project repositories require the shared infrastructure workflow to be public before they can call it. Existing workflows and repository settings also require review to enforce the no-public-PR execution rule.

```yaml
schemaVersion: 1
project: portfolio
workloads:
  web:
    kind: web
    component: web
    port: 3000
    healthPath: /
    domains: [hansteen.dev]
    architecture: amd64
    resourceClass: small
```

| Contract | Value |
| --- | --- |
| Workload kinds | `web`, `worker`, `cron` |
| Health | Web `healthPath`; optional separate `readinessPath` |
| Shutdown | `terminationGracePeriodSeconds`: 30–120; default 30 |
| Resource classes | `small`, `medium`, `large`, `compute`; requests and limits owned by the platform |
| Architecture | `amd64`, `arm64`, `multi`; required native variants must exist |
| Secrets | Named `secretKeys` from a project-scoped Doppler config |
| Registry access | Read-only `GHCR_DOCKER_CONFIG_JSON` in the project's Doppler config |
| Internet egress | Explicit `egress: internet`; public TCP 443 only |
| Internal traffic | Same-project web ports and selected data dependencies |
| Traces | Alloy OTLP at `alloy.observability.svc.cluster.local:4317` or `:4318`; applications require instrumentation |
| Data | Catalog-approved PostgreSQL or Valkey, storage class and explicit recovery objectives |
| Local volumes | Single writer; protected stateful node label; retained PVC |
| Shared files | Catalog-approved `sharedVolumes`; one replica per consumer, one retained local PVC, compatible architecture |
| Migration hook | Approved component, PostgreSQL prerequisite, `compatibility: backward-compatible`, `retrySafe: true`, 300-second deadline |
| Data ordering | Independent `project-data` release becomes ready before application installation and migration |
| Public request headers | Platform-owned Traefik middleware removes client-supplied `X-Admin-Origin` |
| Components | Catalog-owned image per component; all required releases share source revision and workflow run |
| Public build inputs | Workflow `build-arguments` JSON; per-component catalog allowlist; provenance configuration hash |

Portfolio uses separate web, API and worker images. Its complete configuration still requires environment, routing and data mapping; the example is not its migration manifest. No project application is deployed without an accepted release record.

[Application migration inventory](../platform/migrations/apps.json) records source revisions, ports, environment key names, storage, migration commands and outstanding acceptance checks. Portfolio requires a migration-only entrypoint or verified worker startup gate; its API process continues serving after SQLx migrations. Public header removal does not establish an authenticated admin route.

Migration hooks may run again after retries or chart updates. Breaking database or shared-file changes require a coordinated drain, consistent backup, migration and restart. Retained PVCs require a separate backup and tested restore.

## Release flow

| Stage | Boundary |
| --- | --- |
| Project execution | Protected `main` push; numeric owner/repository allowlist before runner allocation |
| Build | Native architecture, ephemeral gVisor runtime, no host sockets or store mounts |
| Publication | GHCR immutable digest; signing and provenance bound to repository, workflow and source |
| Release request | OctoSTS policy; release-only change in this repository |
| Main review | Maintainer review and local validation; no PR-triggered workflow |
| Infrastructure validation | Protected-main checks reverify releases and generated resources |
| Promotion | Successful checks advance `deploy` without force |
| Reconciliation | Namespace-scoped Helm deployment, health checks and rollback |

| GitHub setting | Required value |
| --- | --- |
| `INFRA_CI_IMAGE` | Reviewed, piloted `ghcr.io/fredrir/infra-ci@sha256:…` |
| `INFRA_CI_ARCHITECTURES` | Reviewed JSON array; `["amd64"]` initially, ARM64 only after its native pilot |
| Runner registrations | Per repository and native architecture; numeric ID in runner label |
| Public contributor workflows | Require approval for every external contributor; do not approve execution |
| Untrusted event paths | No `pull_request`, `pull_request_target`, issue-comment or workflow-run execution |
| Registration credentials | Separate GitHub App credential per approved repository scope |
| CI registry credentials | Read-only `ci-registry/.dockerconfigjson` for the `infra-ci` image in each selected `ci-<repositoryId>-<arch>` namespace |
| Runtime credentials | No production kubeconfig, host keys or infrastructure cache signing key in CI |

The CI image bootstrap command is `scripts/ci/bootstrap-image.sh` on a trusted native Linux build host with Docker Buildx. Its output is an OCI archive, not a published or approved image. The real ARC, BuildKit and Nix sandbox pilot must pass before enabling shared workers.

Catalog changes require `python scripts/ci/policy.py write-guards`; `check` rejects guard drift. Optional cache callers explicitly map `NIX_CACHE_READ_TOKEN` and `NIX_CACHE_UPLOAD_TOKEN`; tokens belong to one project cache and signing keys remain outside CI.

## Host enrollment and activation

| Step | Evidence |
| --- | --- |
| Inspect provisioned machines | SSH alias, host key, OS, architecture, disks, routes, kernel features and recovery access |
| Inventory | Verified node ID, adapter, enrollment addresses and administrative public keys |
| Host configuration | Nix `mkPlatformHost` or Ansible inventory; private runtime token files |
| API transport | Three reachable endpoints; cold bootstrap and endpoint loss tests |
| Quorum | Existing CPX22 evacuated before becoming the third server; one-server failure test |
| Worker eligibility | Explicit protected labels after production, stateful and sandbox tests |
| Secrets | Namespace-scoped Doppler/GitHub credentials and out-of-cluster encrypted recovery copy |
| Platform activation | Concrete settings and timestamped evidence accepted by `activation-plan` |
| Application cutover | Restore rehearsal, stop/fence old writer, final transfer, health check and DNS cutover |

```sh
uv run --frozen --group ci python scripts/operations/platform.py bootstrap-plan \
  --repository https://github.com/fredrir/llunde-infra.git \
  --revision "$workflow_revision"

uv run --frozen --group ci python scripts/operations/platform.py activation-plan \
  .infra/activation.json
```

Both commands produce reviewable output without applying it. Activation evidence binds the K3s version, host configuration hashes and CI image. Kernel, runtime, network or pipeline-image changes invalidate the corresponding pilot assumptions.

| Activation input | Value |
| --- | --- |
| `layers` | Explicit layers and their dependencies |
| `settings` | Concrete nonsecret values from `platform/clusters/production/settings.yaml` |
| `secrets` | Namespace, name and key metadata; no credential values |
| `runnerRepositories` | Numeric repository IDs; begin with one pilot repository |
| `runnerArchitectures` | `amd64` initially; add verified `arm64` capacity separately |
| Host evidence | `platform.py host-contract` supplies expected inventory/adapter hashes and API ranges |
| CI evidence | Selected native architectures, sandbox enforcement, API/production isolation and approved image |
| Shared concurrency | Aggregate contention, disk-pressure and node-loss evidence before multiple repository pools |
| Source handoff | Verified `deploy` revision before changing Flux from its bootstrap commit |

The external watchdog uses `platform.watchdog` on NixOS or the `platform_external` Ansible group. Its private runtime config contains HTTPS health targets, optional timestamped heartbeat endpoints, an alert webhook and an optional independent deadman URL. It has no listener or cluster credentials.

## Required capability status

| Priority | Capability | Implemented entrypoint | Live acceptance |
| --- | --- | --- | --- |
| Must | Central ownership | Inventory, catalog, host adapters, DNS import map | Transfer external project Terraform ownership without duplicate state |
| Must | Three K3s servers | Host modules, Ansible and optional provider declarations | Import new CX33s; evacuate CPX22; quorum test |
| Must | CI boundary | Workflow guards, ARC, gVisor and admission | Both native architectures pass real build and isolation tests |
| Must | Portability | Common inventory, mixed OS adapters, Restic/native recovery | Provider/network qualification and alternate-provider restore |
| Must | Project contract | JSON schemas, `infra`, shared chart | Onboard and cut over each application |
| Must | Least privilege | Scoped reconcilers, secret stores, policies and OctoSTS | Identity installation and negative authorization tests |
| Must | Public repository audit | `scripts/audit/publication.py` | Final candidate-head and manual metadata/GitHub review |
| Must | Reproducible releases | Digest/provenance contracts and promotion checks | Signed multi-architecture release through the complete pipeline |
| Must | Data recovery | Native backup jobs and `recovery.py` | Measured RPO/RTO and successful restore with writer fencing |
| Must | Domains and operations | DNS adapter, ingress, telemetry and external watchdog host | DNS/TLS, authenticated Grafana and alert delivery tests |
| Should | Shared workflows | `.github/workflows/project-ci.yml` | Published workflow and project caller updates |
| Should | OctoSTS | `.github/chainguard` and release-request implementation | App installation and scoped exchange test |
| Should | Onboarding command | `infra onboard` | Published workflow reference and approved catalog entry |
| Should | Reusable workloads | `charts/project` | Application health/data checks on K3s |
| Should | Resource governance | Quotas, requests, priorities, bounded ARC and storage | Measure contention, reserve headroom and tune concurrency |
| Should | Signed Nix cache | Attic service and CI cache client | Scoped tokens, signature verification and key/data recovery |
| Should | Trace storage | Alloy, Tempo, S3 settings and Grafana | Instrumented trace round trip and retention checks |
| Should | Maintenance and drift | Pinned versions, Renovate and validation | Scheduled advisory review, restore drills and upgrade pilots |

## Recovery and publication

| Command | Input |
| --- | --- |
| `python scripts/operations/recovery.py etcd-bundle --help` | Snapshot and matching server token |
| `python scripts/operations/recovery.py import-etcd-tar --help` | Host-produced archive imported into a private recovery directory |
| `python scripts/operations/recovery.py verify-bundle --help` | Checksummed encrypted-repository recovery bundle |
| `python scripts/operations/recovery.py postgres-backup --help` | Native PostgreSQL connection and protected passfile |
| `python scripts/operations/recovery.py postgres-restore --help` | Isolated restore target and expected-table verification |
| `python scripts/audit/publication.py --help` | Installed Gitleaks binary; reachable history and tracked/untracked source |

Recovery credentials must survive loss of the cluster. Database backups remain suspended until their credentials and restore evidence are reviewed. Raw database directories are not a cross-architecture migration format.

The automated publication audit writes redacted reports under ignored `.infra/audit`. It does not replace review of encrypted payloads, public-key identities, infrastructure metadata, GitHub artifacts or repository settings.
