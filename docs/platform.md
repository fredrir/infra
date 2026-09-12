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
| GitHub Actions | Infrastructure, portfolio and Y disabled; no active infrastructure runs when disabled |

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
| Transport verification |07/08 routes approved;09 reached both private TCP6443 fixtures through either router; controlled advertisement withdrawal passed, fixtures removed |
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
uv run --frozen --group ci python scripts/operations/platform_cli.py check
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
| Kata Nix sandbox | Offline amd64 VM passes ordinary derivation, UID/GID mapping and read-only store-input checks |
| Kata guest keyrings | Required join, describe and permission operations pass; key reading and add/request-key remain denied |
| Kata BuildKit | COPY passes; nested runc's cgroup device-filter query is blocked by the guest BPF policy before RUN |
| Kata lifecycle | Native teardown reports a busy cgroup; outer cleanup removes all owned processes, mounts and files; no ARC integration or production selection |

[CI runtime pilot](research/ci-runtime-pilot.md) records scope, security-version review and remaining gates. Private logs and source hashes are retained under `.infra/ci-kata-pilot/evidence/`. No ordinary-runtime, disabled-sandbox or software-emulation fallback is authorized by these results.

Catalog changes require `python scripts/ci/policy.py write-guards`; `check` rejects guard drift. Optional cache callers explicitly map `NIX_CACHE_READ_TOKEN` and `NIX_CACHE_UPLOAD_TOKEN`; tokens belong to one project cache and signing keys remain outside CI.

## Temporary fredrir-05 evacuation target

| Live status, 2026-09-12 | Evidence scope |
| --- | --- |
| fredrir-09 preparation | Podman5.7.0; locked rootless users;4GiB aggregate user-slice memory limit; corrected units installed and serving |
| Images | Six exact source images preloaded; runtime configIDs and `Pull=never`; no registry credential required |
| Secrets | Three scoped05 runtime secrets relayed over verified SSH into an age-encrypted bundle; regeneration after09 reboot passed |
| Start guards | Installed on05 and09; eight native manager probes verify manual starts; systemd automatic restarts bypass these conditions and require separate inhibition |
| Restart inhibition | Native09 probe and fenced05 export passed; owned `Restart=no` prevents Valkey restart; source recovery restores the original policy |
| Application units | Nine corrected candidate files installed on09; target application and connector started after fresh paired restore; all three user managers have linger enabled |
| Data rehearsal | PostgreSQL17.10 logical restore and Valkey8.1.9 RDB restore; synthetic stopped AOF preserves bytes, ownership and TTL |
| Native Valkey shutdown | Disposable09 probe passes zero kernel capabilities, no-new-privileges, bounded private tmpfs, native server exit0 and event verification; container removed and application states unchanged |
| Frontend routing | Direct frontend and cross-user Caddy responses match; only loopback8081/8085/9101; diagnostic containers removed and ports released |
| Final paired export | Fenced05 PostgreSQL dump and stopped Valkey archive sealed and transferred to09; source and destination hashes match |
| Off-host backup | Fresh forward and reverse pairs backed up to S3 and independently restored on Mac; archive, files, source manifest and candidate bindings match |
| Recurring backups | Hourly09 timer enabled; native timer-triggered backup completed and restricted06 receiver matched its snapshot; the next hourly elapse is recorded |
| Native paired restore | Fresh forward pair restored and promoted on09; fresh reverse pair restored and promoted on05; PostgreSQL schema and Valkey AOF, ownership and byte-preservation checks pass; temporary containers removed |
| Shutdown verification | PostgreSQL server exit0 accepted despite shutdown client137; actual server failure and OOM remain rejected;26 focused tests pass |
| Source recovery | Fresh reverse data restored on05; private/public acceptance and sole05 connector verified; user confirmed existing-account sign-in and account content; four source timers restored; conservative full recovery641.4s (10m41.4s); scheduled external monitor passed at07:44:13 UTC |
| Scheduled updates | Automatic APT upgrade overlapped an earlier cutover attempt; exact requester of09 user-manager stops remains unconfirmed |
| Update window | Native09 rehearsal passed; actual cutover paused both APT timers and restored their original active/enabled states after recovery |
| Backend compatibility | Both data and outbound networks were attached, but only the internal data network had DNS enabled; api.doppler.com returned NXDOMAIN and startup failed |
| Candidate correction | Nine-file candidate adds backend-only DNS-enabled egress; datastore isolation, images, ports and limits preserved; installed without application starts |
| Native network verification | Original NXDOMAIN reproduced; corrected DNS and verified HTTPS401 passed; internal-only external DNS and routing denied; disposable objects independently confirmed removed |
| Native backend rehearsal | Isolated09 PostgreSQL and Valkey fixtures pass migrations, registration, sign-in, authenticated identity, logout invalidation and access rejection; actual cookie attributes verified; cleanup independently confirmed; APT timers restored |
| Rehearsal boundary | Synthetic data and local auth checks; no production-data compatibility, Doppler retrieval, browser transport or target existing-account acceptance |
| Target reset | Native prepare/finalize passed;36 completed archival/staging events; original database directory identities preserved; old configuration and fences archived; APT timers restored after confirmed completion |
| Cutover preparation | Frozen corrected bundle delivered to05/09; native preflight verifies source services active, target units dormant, exact images, loaded guards and absent approval/fence markers |
| Target activation | Fresh paired export restored/promoted on09; off-host backup independently recovered on Mac with exact hashes; private and public routes pass; user confirmed existing-account sign-in and account content on09 |
| Origin verification | Sole post-reboot connector uses09 public IPv4 `85.190.100.72`; three fresh provider observations match its native identity; pre-reboot IPv6 evidence and rejected address expectations remain separate |
| Startup persistence | One controlled reboot completed; all six services restarted with exact images; three secrets regenerated before their user managers started; private and public routes passed independent review |
| Installed-service backup | Native09 job completed successfully; fresh PostgreSQL and Valkey recovery points follow user acceptance; exact snapshot independently downloaded on Mac with matching archive and file hashes; restricted06 receiver matches the snapshot |
| Native online recovery | Archie restored PostgreSQL17.10 and Valkey8.1.9 from the independently downloaded snapshot; two sessions invalidated in the isolated copy; zero remaining session keys; empty private container/image store, removed data and ended processes independently verified |
| Pre-reboot recovery point | Fresh installed-service snapshot completed and independently downloaded on Mac; exact archive, three files, candidate/images and06 receiver match; native recovery evidence identifies its earlier snapshot separately |
| Backup observation | Immediate process check failed after dispatch; native journal independently confirms the single invocation completed successfully; no duplicate start |
| Preparation checks | Owned private data parent created only when absent and required before cutover; unsafe network overrides rejected;224 evacuation tests pass |
| Post-reboot recovery | Installed service and restricted06 receiver match the new-boot snapshot; Mac verified all three files; Archie restored both datastores with independently verified cleanup; user confirmed existing-account sign-in and content after reboot |
| Backup freshness | Independent06 watchdog checks the09 recovery point against a two-hour maximum age; scheduled health check passed with no failures |
| Production boundary | Target09 serves public routes with verified startup, hourly backups and independent monitoring; source05 remains fenced with its original data preserved; both APT timers restored; no K3s activation |

Private receipts remain under `.infra/evacuation-05/`. Bounded rehearsal timings do not measure production downtime.

## Host enrollment and activation

| Step | Evidence |
| --- | --- |
| Inspect provisioned machines | SSH alias, host key, OS, architecture, disks, routes, kernel features and recovery access |
| Inventory | Verified node ID, adapter, enrollment addresses and administrative public keys |
| Host configuration | Nix `mkPlatformHost` or Ansible inventory; private token files available at every boot |
| fredrir-05 candidate | [Host configuration](../hosts/fredrir-05/default.nix) preserves configured storage, identity and administrator access; local contract passes; Linux closure build and host activation remain pending; K3s is disabled |
| API transport | Three reachable endpoints; cold bootstrap and endpoint loss tests |
| Quorum | Existing CPX22 evacuated before becoming the third server; one-server failure test |
| Worker eligibility | Explicit protected labels after production, stateful and sandbox tests |
| Secrets | Namespace-scoped Doppler/GitHub credentials and out-of-cluster encrypted recovery copy |
| Platform activation | Concrete settings and timestamped evidence accepted by `activation-plan` |
| Application cutover | Restore rehearsal, stop/fence old writer, final transfer, health check and DNS cutover |

```sh
uv run --frozen --group ci python scripts/operations/platform_cli.py bootstrap-plan \
  --repository https://github.com/fredrir/llunde-infra.git \
  --revision "$workflow_revision"

uv run --frozen --group ci python scripts/operations/platform_cli.py activation-plan \
  .infra/activation.json
```

Both commands produce reviewable output without applying it. Activation evidence binds the K3s version, host configuration hashes and CI image. Kernel, runtime, network or pipeline-image changes invalidate the corresponding pilot assumptions.

| K3s credential | Storage |
| --- | --- |
| Ansible hosts | `/var/lib/platform/credentials/k3s-server-token` and `k3s-agent-token`; root0700 directory, root0400/0600 files supplied separately |
| NixOS hosts | `/run/secrets/k3s-server-token` and `k3s-agent-token`; secret provisioning must regenerate them before K3s at each boot |
| Worker authority | Agent token only; no server credential delivered |
| Joining nodes | Secure token format containing the verified cluster CA hash; first-server initialization uses separate random short server/agent credentials |
| Recovery | Encrypted off-host copy of the server token with each matching etcd snapshot; never put token values in inventory, Ansible variables or command arguments |

K3s writes secure server and agent tokens after initialization. Retrieve them privately from the first server for subsequent joins; the [server token is also required to decrypt datastore bootstrap data during recovery](https://docs.k3s.io/cli/token). Runtime-only files on Ansible hosts require an explicit boot-time provider if overriding the persistent defaults.

| Activation input | Value |
| --- | --- |
| `layers` | Explicit layers and their dependencies |
| `settings` | Concrete nonsecret values from `platform/clusters/production/settings.yaml` |
| `secrets` | Namespace, name and key metadata; no credential values |
| `runnerRepositories` | Numeric repository IDs; begin with one pilot repository |
| `runnerArchitectures` | `amd64` initially; add verified `arm64` capacity separately |
| Host evidence | `platform_cli.py host-contract` supplies expected inventory/adapter hashes and API ranges |
| CI evidence | Selected native architectures, sandbox enforcement, API/production isolation and approved image |
| Shared concurrency | Aggregate contention, disk-pressure and node-loss evidence before multiple repository pools |
| Source handoff | Verified `deploy` revision before changing Flux from its bootstrap commit |

The external watchdog uses `platform.watchdog` on NixOS or the `platform_external` Ansible group. Its private runtime config contains HTTPS health targets, optional timestamped heartbeat endpoints, exactly one email/webhook alert transport and an optional independent deadman URL. It has no listener or cluster credentials. [Email credential and deployment commands](mail-alerts.md).

| Watchdog configuration | Value |
| --- | --- |
| Credential file | Root-owned0400/0600 source passed through systemd `LoadCredential`; regular private JSON, maximum64KiB |
| `targets` | Up to32 combined health/heartbeat checks; each has unique `name`, HTTPS `url`, optional `expectedStatus` and `authorization` |
| `heartbeats` | HTTPS JSON endpoint, dotted `timestampField`, `maxAgeSeconds`60–604800 |
| `alertWebhook` | HTTPS URL; mutually exclusive with `alertEmail` |
| `alertEmail.host` | SMTP hostname; no URL, embedded port or credential |
| `alertEmail.port` | Explicit integer1–65535; typically465 for implicitTLS or587 for STARTTLS |
| `alertEmail.tls` | `implicit` or `starttls`; certificate and hostname verification required, TLS1.2 minimum, no cleartext fallback |
| `alertEmail.username` | Doppler `llunde/ops/PLATFORM_WATCHDOG_SMTP_USERNAME` |
| `alertEmail.password` | Doppler `llunde/ops/PLATFORM_WATCHDOG_SMTP_PASSWORD` |
| `alertEmail.from` | `alerts@fredrir.com`; SES identity and sender permission must be verified before delivery |
| `alertEmail.to` | One bare ASCII operator address; no display name, list or custom headers |
| SMTP endpoint | `email-smtp.eu-north-1.amazonaws.com`, port587, `starttls` |
| IAM / secret ownership | OpenTofu `module.platform_mail` owns the scoped IAM user; SMTP credentials remain outside state in Doppler |
| `deadmanURL` | Optional independent HTTPS confirmation endpoint; called only when every check is healthy |
| Delivery bounds | SMTP socket timeout5s, message16KiB, entire watchdog service180s |
| Delivery acknowledgement | Failure changes and recovery trigger alerts; failed delivery leaves prior state intact for retry; SMTP acceptance requires a separate inbox test |
| Host status |06 also hosts existing Y services; watchdog deployment must preserve those workloads |

SMTP uses Python's [TLS-capable SMTP clients](https://docs.python.org/3/library/smtplib.html) with a [verified default TLS context](https://docs.python.org/3/library/ssl.html#ssl.create_default_context). Credentials and recipient belong only in the private runtime JSON; repository examples and logs must contain no credential values.

## Required capability status

| Priority | Capability | Implemented entrypoint | Live acceptance |
| --- | --- | --- | --- |
| Must | Central ownership | Inventory, catalog, host adapters, DNS import map | Transfer external project Terraform ownership without duplicate state |
| Must | Three K3s servers | Host modules, Ansible and provider declarations | CX33 adoption complete; evacuate CPX22, complete transport and test quorum |
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
