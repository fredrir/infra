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

## Production OpenTofu

| Name | Value |
| --- | --- |
| Inputs | [production.tfvars.json](../tofu/production.tfvars.json); required for every full production plan |
| Runtime | OpenTofu 1.12.6; configuration floor 1.10 |
| Locked providers | hcloud 1.68.0; AWS 5.100.0; Cloudflare 5.23.0 |
| Backend | S3 `llunde-pyparser-bucket`, key `tofu-state/infra.tfstate`, `eu-north-1`; encryption and locking enabled |
| Credentials | `HCLOUD_TOKEN`, `CLOUDFLARE_API_TOKEN`, AWS credential chain; export temporary credentials for AWS login sessions |
| Token input | `hcloud_token: null` uses the provider environment; no credential values belong in tfvars |
| Imported servers | fredrir-07/08; verified import IDs retained declaratively |
| Existing control plane | fredrir-05 network attachment and placement remain separate reviewed changes |
| Review | Full saved plan; resource identities, replacements, DNS, access CIDRs, private routes and state changes |

```sh
umask 077
mkdir -p .infra/plans
tofu -chdir=tofu fmt -check -recursive
tofu -chdir=tofu init -lockfile=readonly -input=false
tofu -chdir=tofu validate
tofu -chdir=tofu plan -input=false -lock-timeout=30s \
  -var-file=production.tfvars.json \
  -out=../.infra/plans/production.tfplan
```

Apply only the reviewed saved plan after approval. Preserve a protected state backup and plan hash before applying; inspect partial changes and replan before recovery. Existing servers must be offline when joining a [Hetzner placement group](https://docs.hetzner.com/cloud/placement-groups/faq/), with restart and SSH verification included in the approved maintenance procedure. Do not restore old state over successful provider changes or rebuild servers as a rollback shortcut.

## Transport bootstrap

| Name | Value |
| --- | --- |
| Entry point | [tailscale-bootstrap.yml](../ansible/tailscale-bootstrap.yml); provider SSH access before any tailnet address exists |
| Supported hosts | Ubuntu 24.04/26.04 or Debian 13; systemd; amd64/arm64; kernel TUN support |
| Version | Tailscale 1.102.4; shared architecture-specific archive checksums |
| Authority | Applied [tailnet policy](../tailscale/policy.hujson) and valid auth key authorized for exactly one reviewed role tag |
| Role tag | `tag:platform-control` or `tag:platform-worker`; no user-owned or additional-tag identity accepted |
| Hostname | Inventory host name `fredrir-NN`; reconciled explicitly in Tailscale |
| Credential | Existing remote regular file under `/run`; root-owned `0400`/`0600`; contents never passed through Ansible variables |
| Result | Connected tagged identity and reported IPv4; Tailscale SSH disabled; no advertised routes or exit-node service |
| Accepted routes | Workers accept routes; control-plane hosts reject peer routes to preserve their directly connected private network ([HA guidance](https://tailscale.com/docs/how-to/set-up-high-availability)) |
| Repeat | Existing matching identity reused; preferences reconciled on drift; runtime key file still required |
| Boundary | Existing K3s artifacts rejected; no K3s installation, inventory enrollment or capability approval |

Create a local inventory at `.infra/bootstrap.yml` using a verified provider SSH alias and host key:

```yaml
tailscale_bootstrap:
  hosts:
    fredrir-07:
      ansible_host: fredrir-07
      ansible_user: root
      platform_architecture: amd64
      platform_tailscale_bootstrap_tags: [tag:platform-control]
      platform_tailscale_auth_key_file: /run/secrets/tailscale-auth-key
```

After reviewing the applied policy, key tag authority and target host, run:

```sh
ANSIBLE_CONFIG=ansible/ansible.cfg uv run --frozen --group ci \
  ansible-playbook -i .infra/bootstrap.yml ansible/tailscale-bootstrap.yml \
  --limit fredrir-07 \
  -e '{"platform_tailscale_bootstrap_approved":true}'
```

The approval flag records an operator assertion; it does not apply or verify the remote policy. Authentication uses the [pinned CLI file-key interface](https://github.com/tailscale/tailscale/blob/v1.102.4/cmd/tailscale/cli/up.go), and enrollment verifies the assigned role tag. Remove the temporary key through the approved credential procedure after use. Review the assigned address with the node's private interface, admin keys and host checks before updating enrollment; three reviewed control-plane addresses are required for K3s configuration.

| Enrollment key | Value |
| --- | --- |
| Helper | [tailscale_enrollment.py](../scripts/operations/tailscale_enrollment.py) |
| Doppler location | Project `llunde`, config `ops` |
| OAuth credentials | `TAILSCALE_ENROLL_CLIENT_ID`, `TAILSCALE_ENROLL_CLIENT_SECRET`; alternatively `TS_API_CLIENT_ID`, `TS_API_CLIENT_SECRET` |
| OAuth client | Scope `auth_keys`; select only `tag:platform-enrollment`; never select both node-role tags |
| OAuth request | Scope `auth_keys`; exactly one `tags` value, `tag:platform-control` or `tag:platform-worker` |
| Auth key | One-time, non-ephemeral, preauthorized, ten-minute expiry; matching creation and retrieved metadata required |
| Delivery | Verified SSH alias and configured user; root or passwordless `sudo -n`; key through stdin; atomic non-overwriting `0400` runtime file and metadata receipt |
| Runtime path | Default `/run/secrets/tailscale-auth-key`; `--key-file` must match the bootstrap inventory |
| Local output | Node, role, key ID, expiry and path; no credential values or local credential files |
| Failure | Revoke the created key; remove only its matching remote receipt and unchanged key; report unconfirmed cleanup |

Use Tailscale's [tag-owner delegation](https://tailscale.com/docs/features/oauth-clients#generating-long-lived-auth-keys): the admin-owned enrollment tag owns exactly the two node roles and has no network grants. The helper requests a separate token narrowed to one owned role and checks the auth key's exact tag in both creation and retrieved metadata. Nodes never receive the enrollment tag or OAuth secret. A client carrying both node-role tags cannot issue either single-role key; do not retry with both tags on a node.

```sh
doppler run -p llunde -c ops \
  --only-secrets TAILSCALE_ENROLL_CLIENT_ID,TAILSCALE_ENROLL_CLIENT_SECRET -- \
  uv run --frozen python scripts/operations/tailscale_enrollment.py create-deliver \
  --node fredrir-07 --role control
```

Run the bootstrap playbook immediately for that one host before creating the next key. Retain the returned non-secret `keyId` for cleanup:

```sh
doppler run -p llunde -c ops \
  --only-secrets TAILSCALE_ENROLL_CLIENT_ID,TAILSCALE_ENROLL_CLIENT_SECRET -- \
  uv run --frozen python scripts/operations/tailscale_enrollment.py revoke-unused \
  --node fredrir-07 --role control --key-id "$key_id"
```

Cleanup handles unused, expired or already-consumed keys. Revoking a key [does not deauthorize an enrolled node](https://tailscale.com/docs/features/access-control/auth-keys#revoke-an-auth-key). If cleanup cannot confirm revocation, inspect the reported request description before retrying; the helper never retries key creation automatically.

## Tailnet access contract

| Source | Destination | Protocol / ports |
| --- | --- | --- |
| Macie, Archie | Platform control and worker tags | TCP 22; OpenSSH key authentication |
| Macie, Archie, platform nodes | `10.60.0.5`, `10.60.0.7`, `10.60.0.8` | TCP 6443; Kubernetes authentication |
| Platform nodes | Platform tags and the three private control addresses | TCP 9100, 10250; telemetry and authenticated kubelet |
| Platform nodes | Platform control and worker tags | UDP 8472; Flannel VXLAN over Tailscale |
| Every Tailnet identity | Platform etcd | No Tailnet grant; TCP 2379/2380 stays on Hetzner's private interface |
| Legacy CI tag, untagged users, pod addresses | Platform endpoints | No new grant; existing legacy rules remain |
| Enrollment tag | Every endpoint | No network or SSH grant; credential delegation only |

| Name | Value |
| --- | --- |
| Policy | [policy.hujson](../tailscale/policy.hujson); enrollment tag owned only by `autogroup:admin`; node roles owned by admins and the enrollment tag |
| Private routes | Optional `10.60.0.5/32`, `10.60.0.7/32`, `10.60.0.8/32`; complete identical set on reviewed control routers |
| Route approval | Manual; no automatic approvers, pod/service CIDRs or exit-node routes |
| Control-plane routing | Reject imported routes; preflight requires each private peer route to use the private interface |
| Runtime boundary | Host tags identify machines; CI NetworkPolicy must block node/API egress before NAT can inherit a host identity |
| Activation | Compare current remote policy, retain unrelated rules, run native policy validation, review the diff and apply within the authorized scope using the current ETag |
| Recovery | Preserve the prior policy; retain provider SSH access while testing new identities and route failover |

The policy includes protocol-specific [native allow/deny assertions](https://tailscale.com/docs/reference/syntax/policy-file#tests). Local tests evaluate the supported ACL subset against explicit fixtures; native validation and live reachability remain separate gates. Redundant subnet routers must advertise identical prefixes and avoid accepting their own routes through peers ([Tailscale HA guidance](https://tailscale.com/docs/how-to/set-up-high-availability)).

```sh
uv run --frozen --group ci python -m unittest discover \
  -s tests/operations -p test_tailnet.py -v
```

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
helm lint charts/project-data -f tests/infra/fixtures/data-values.yaml
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
| Capacity checks | Catalog-owned quota profiles; steady state, concurrent jobs, migration and rollout capacity checked before rendering |
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
| Build | Native architecture; configured gVisor profile remains blocked by runtime compatibility evidence; no host sockets or store mounts |
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

### CI runtime evidence

| Check, 2026-09-12 | Result |
| --- | --- |
| Pipeline image on Archie | Native amd64 image built privately; tooling starts; image remains unpublished |
| Pinned gVisor, configured non-root profile | Nix sandbox namespace probe fails; RootlessKit multi-UID mapping fails |
| Pinned gVisor, diagnostic guest-root profile | BuildKit passes Dockerfile `RUN`, non-root `USER`, numeric ownership and OCI export; this profile is not admitted by current policy |
| Nix 2.35.2 with pinned gVisor | Ordinary sandboxed derivations fail on unsupported `SIOCSIFFLAGS`; additional capabilities do not resolve the missing syscall interface |
| fredrir-09 KVM | One vCPU executes `MOV AX,42; HLT`; KVM exit reason 5 and AX 42; temporary resources released |
| Kata | Candidate only; no installation, VM build test, ARC integration or production selection |

[CI runtime pilot](research/ci-runtime-pilot.md) records scope, security-version review and remaining gates. Private logs and source hashes are retained under `.infra/ci-kata-pilot/evidence/`. No ordinary-runtime, disabled-sandbox or software-emulation fallback is authorized by these results.

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

The external watchdog uses `platform.watchdog` on NixOS or the `platform_external` Ansible group. Its private runtime config contains HTTPS health targets, optional timestamped heartbeat endpoints, exactly one email/webhook alert transport and an optional independent deadman URL. It has no listener or cluster credentials.

| Watchdog configuration | Value |
| --- | --- |
| Credential file | Root-owned0400/0600 source passed through systemd `LoadCredential`; regular private JSON, maximum64KiB |
| `targets` | Up to32 combined health/heartbeat checks; each has unique `name`, HTTPS `url`, optional `expectedStatus` and `authorization` |
| `heartbeats` | HTTPS JSON endpoint, dotted `timestampField`, `maxAgeSeconds`60–604800 |
| `alertWebhook` | HTTPS URL; mutually exclusive with `alertEmail` |
| `alertEmail.host` | SMTP hostname; no URL, embedded port or credential |
| `alertEmail.port` | Explicit integer1–65535; typically465 for implicitTLS or587 for STARTTLS |
| `alertEmail.tls` | `implicit` or `starttls`; certificate and hostname verification required, TLS1.2 minimum, no cleartext fallback |
| `alertEmail.username` | SMTP user from private credential source; existing Grafana key `GF_SMTP_USER` maps here |
| `alertEmail.password` | SMTP password from private credential source; existing Grafana key `GF_SMTP_PASSWORD` maps here |
| `alertEmail.from` | `alerts@fredrir.com`; SES identity and sender permission must be verified before delivery |
| `alertEmail.to` | One bare ASCII operator address; no display name, list or custom headers |
| SMTP endpoint | `email-smtp.eu-north-1.amazonaws.com`, port587, `starttls`; existing credential source `/run/secrets/observability-smtp.env` on04 |
| Existing source metadata | `secrets/observability-smtp.yaml`, key `env`; SES SMTP IAM permission is not declared in this repository |
| `deadmanURL` | Optional independent HTTPS confirmation endpoint; called only when every check is healthy |
| Delivery bounds | SMTP socket timeout5s, message16KiB, entire watchdog service180s |
| Delivery acknowledgement | Failure changes and recovery trigger alerts; failed delivery leaves prior state intact for retry; SMTP acceptance requires a separate inbox test |
| Host status |06 also hosts existing Y services; watchdog deployment must preserve those workloads |

SMTP uses Python's [TLS-capable SMTP clients](https://docs.python.org/3/library/smtplib.html) with a [verified default TLS context](https://docs.python.org/3/library/ssl.html#ssl.create_default_context). Credentials and recipient belong only in the private runtime JSON; repository examples and logs must contain no credential values.

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
| `python scripts/operations/recovery.py volume-export --help` | Drained shared files, fresh operator assertion and paired PostgreSQL bundle |
| `python scripts/operations/recovery.py volume-verify --help` | Bounded archive, project/volume identity and paired database checksum |
| `python scripts/operations/recovery.py volume-restore --help` | New private destination; no overwrite or link extraction |
| `python scripts/audit/publication.py --help` | Installed Gitleaks binary; reachable history and tracked/untracked source |

Recovery credentials must survive loss of the cluster. Database backups remain suspended until their credentials and restore evidence are reviewed. Raw database directories are not a cross-architecture migration format.

Shared-file export requires all writers to stay drained through the paired database dump and final transfer. Its timestamped drain assertion records an operator check; the command does not stop workloads. Exported bundles require separate upload to the independent encrypted Restic repository, restored runtime ownership and an application restore rehearsal.

The automated publication audit writes redacted reports under ignored `.infra/audit`. It does not replace review of encrypted payloads, public-key identities, infrastructure metadata, GitHub artifacts or repository settings.
