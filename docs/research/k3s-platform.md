# K3s platform

| Name | Value |
| --- | --- |
| Status | Architecture research; current configuration and operation are in [platform.md](../platform.md) |
| Scope | All projects; three dedicated control-plane servers; shared production/CI workers; built-in Kubernetes scheduling |
| Inventory | [Nodes](../nodes.md), [domains](../domains.md) |
| Fleet research | [Provider offers, capacity and purchase gates](fleet-research.md) |
| Version/security review | [Versions and security](versions-security.md) |
| Source review | 2026-09-12 |

## MoSCoW

| Priority | Capability | Required outcome |
| --- | --- | --- |
| Must | Central infrastructure ownership | One owner for each machine, DNS record, route, deployment and data lifecycle |
| Must | Three production K3s servers | Colocated embedded-etcd quorum; one-server failure exercise |
| Must | CI execution boundary | No untrusted PR execution; sandbox compatibility gate before CI shares production workers |
| Must | Provider portability | Common node contract, mixed Linux support, replaceable provider integrations and cross-provider restore |
| Must | Project contract | Small validated configuration; approved repository, namespace, domains, resource class and capabilities |
| Must | Least privilege | Scoped cross-repository authentication, runtime secrets, project isolation and authenticated administration |
| Must | Public repository audit | Review current contents and Git history before any visibility change |
| Must | Reproducible releases | Immutable image digests, verified signatures/provenance, gated promotion, health verification and application rollback |
| Must | Data recovery | Explicit RPO/RTO, off-host backups, restore rehearsal and single-writer migration |
| Must | Domains and operations | Central DNS/TLS ownership; metrics, logs, alerts and an independent watchdog |
| Should | Shared workflows | Versioned build, project-test hooks, scanning, publication and release promotion |
| Should | OctoSTS release requests | Narrow cross-repository authorization; separate from ARC runner-registration credentials |
| Should | Onboarding CLI | Validate and generate project configuration and a reviewable onboarding change |
| Should | Reusable workloads | Shared Helm/Kustomize interfaces for web services, workers, scheduled jobs and stateful dependencies |
| Should | Resource governance | Requests, limits, quotas, capacity measurements, global CI budget and storage retention |
| Should | Signed Nix cache | Scoped writers, trusted signatures, retention and an independent bootstrap path |
| Should | Trace storage | Bounded ingestion, retention and Grafana correlation with logs and metrics |
| Should | Maintenance and drift | Pinned versions, dependency updates, advisory review and drift detection |
| Could | Additional platform services | Preview environments with expiry, self-hosted registry and capacity/cost reporting |
| Could | Home hosting | Eligible workers after connectivity, availability and recovery checks |
| Could | Custom dashboard | Read existing inventory and telemetry APIs |
| Won't | Custom scheduling or operator | No custom scheduler or bespoke workload operator |
| Won't | Distributed quorum/storage across home and clouds | No home-dependent quorum or WAN-replicated block storage |

## Product proposals

| Capability | PROPOSED implementation | Boundary |
| --- | --- | --- |
| Machines and networks | OpenTofu provider adapters | Provider resources and DNS; manually supplied machines use the same inventory contract |
| Hosts | NixOS modules; Ansible candidate for supported Ubuntu/Debian | OS-specific adapters own host access, K3s, networking, disks and bootstrap |
| Production runtime | K3s with containerd | Podman remains only during workload migration |
| Cluster reconciliation | Flux | Application and platform Kubernetes resources |
| Workload interface | Shared Helm chart plus Kustomize composition | Typed capabilities; no arbitrary resource injection |
| Scheduling | Kubernetes scheduler | Requests, affinity, taints, topology spread and priority classes |
| CI | Actions Runner Controller | Repository-scoped ephemeral runners for the personal account |
| Build isolation | Configured gVisor `systrap` profile is blocked by Nix compatibility evidence; Kata is an unselected alternative | Validate real Nix/container builds on both architectures; no privileged DinD or host daemon mounts on shared workers |
| Cross-repository releases | OctoSTS | Scoped release requests; ARC uses its own GitHub App authentication |
| Images | GHCR | Digest-based publication and deployment; registry replacement remains possible |
| Nix cache | Attic candidate | Adoption depends on maturity review, restore checks and signing isolation |
| Secrets | SOPS bootstrap and Doppler runtime secrets; External Secrets Operator candidate | Namespace-scoped stores and named keys; K3s secrets encryption and RBAC |
| Observability | Alloy, Prometheus, Loki, Grafana and monolithic Tempo | Bounded resources, retention and private ingestion |
| Public ingress | In-cluster cloudflared and Flux-managed pinned Traefik | Tunnel to Traefik ClusterIP; packaged Traefik/ServiceLB disabled on every server |

NixOS already exposes K3s roles, runtime token files, node identity and kubelet configuration in the [pinned module](https://github.com/NixOS/nixpkgs/blob/b6018f87da91d19d0ab4cf979885689b469cdd41/nixos/modules/services/cluster/rancher/default.nix). [Flux](https://fluxcd.io/flux/concepts/) reconciles cluster resources from Git and can manage its own installation.

[Attic](https://docs.attic.rs/) describes itself as an early prototype; cache availability must not determine whether the platform can recover. [Tempo monolithic mode](https://grafana.com/docs/tempo/latest/reference-tempo-architecture/deployment-modes/) suits modest trace volumes without a Kafka dependency; production object storage and explicit ingestion/query budgets remain required design inputs.

[External Secrets Operator's Doppler provider](https://external-secrets.io/latest/provider/doppler/) supports tokens scoped to one config and named-key retrieval. Enable [K3s secrets encryption](https://docs.k3s.io/security/secrets-encryption) explicitly; Kubernetes Secret objects alone do not establish encryption at rest.

## Topology and capacity

| Machine | Proposed role | Capacity condition |
| --- | --- | --- |
| Two new CX33, IDs unassigned, 8 GB / 4 shared vCPU each | K3s control plane and etcd only | Same location/private network as CPX22; separate physical hosts |
| `fredrir-05`, CPX22, 4 GB / 2 vCPU | Third control plane and etcd only | Evacuate existing applications/data before conversion; measure headroom |
| `fredrir-04`, CCX23, 16 GB / 4 dedicated vCPU | Shared production and CI worker | Sandbox, resource and storage eligibility gates |
| New one.com XXL, ID unassigned, 32 GB / 16 vCPU | Shared production and CI worker | Provider/kernel verification and combined database/build I/O trial |
| New HidenCloud SAR-Torrent, ID unassigned, ARM64, 32 GB / 16 shared vCPU | Shared production and native ARM CI worker | Provider terms, kernel and complete ARM build/runtime compatibility gates |
| `fredrir-06`, Linode 1 GB / 1 core | Independent watchdog and lightweight cron | Outside the cluster; no quorum or recovery-capacity claim |
| `fredrir-03` | Future eligible worker | Never an etcd quorum member |
| Fourth home computer, ID unassigned | Future eligible worker | Hardware and availability unassigned; never an etcd quorum member |
| macie / `fredrir-01`, archie / `fredrir-02` | Administration and development | No hosting dependency |

K3s requires at least 2 cores/2 GB per server and 1 core/512 MB per agent before applications. The 1 GB Linode is unsuitable as a control-plane server. [K3s requirements](https://docs.k3s.io/installation/requirements)

| Name | Requirement |
| --- | --- |
| Control-plane location | `hel1`, matching the existing Hetzner configurations; verify actual placement |
| Control-plane network | Private network for the three colocated etcd members; provider-specific implementation |
| Physical placement | Spread placement group; adding existing servers requires an offline maintenance window |
| Worker network | Encrypted cross-provider pod traffic; verified routes, MTU, DNS and partition behavior |
| CI network | Separate namespaces and identities; deny production data, node/admin endpoints and unrelated namespaces |
| Measurements | Peak/resident memory, CPU steal, etcd fsync latency, disk usage, I/O contention, network throughput and representative build demand |
| Allocatable resources | Reserve OS and Kubernetes overhead; requests/limits cover all application containers |
| Failure budget | Critical requests fit surviving eligible workers after each worker failure; calculate per architecture and storage class |
| Worker capacity | 80 GB raw RAM before overhead; neither architecture nor node-local storage is interchangeable |
| Batch work | Lower priority than production; bounded concurrency and disk use; may pause during recovery |
| Placement | Capability labels, architecture affinity, topology spread, requests and quotas; roles remain flexible |
| Migration storage | Budget simultaneous Podman/containerd images, old data and restored data |

[Hetzner spread placement groups](https://docs.hetzner.com/cloud/placement-groups/faq/) separate physical hosts, not sites. [Kubernetes reservations](https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources/) keep OS/runtime requirements out of pod allocatable capacity; reservation values require measurement.

## Availability boundaries

| Failure | Expected behavior | Additional requirement |
| --- | --- | --- |
| One control-plane server | Two etcd members preserve quorum | Reachable API backends and tested cold registration |
| Node holding local data | API remains available; affected data stays unavailable | Restore or separately designed database/storage replication |
| Largest production worker | Critical services fit on remaining nodes | Replica placement, requests, data access and recovery exercise |
| Shared worker | Affects its production pods and CI jobs | Placement, recovery capacity and CI backpressure; no claim of independent failure domains |
| ARM worker | Native ARM CI and ARM-only workloads lose capacity | Queue/recover; tested multiarch workloads may move to eligible x86 nodes |
| Home network | Eligible jobs stop/retry; production quorum survives | Idempotent work and deliberate availability classes |
| Hetzner site | Control plane unavailable; remote workers are not an independent cluster | Existing pods may continue; no reliable rescheduling/reconciliation until quorum recovers |
| Registry or Nix cache | Running workloads continue where possible | Independent access to bootstrap artifacts and backup credentials |

Three embedded-etcd servers tolerate one server loss. K3s requires colocated, privately reachable etcd servers; remote agents are possible, but increased latency affects the cluster and the built-in Tailscale VPN integration is experimental. [Embedded-etcd HA](https://docs.k3s.io/datastore/ha-embedded), [distributed deployments](https://docs.k3s.io/networking/distributed-multicloud)

## API and bootstrap

| Component | Owner | Configuration requirement |
| --- | --- | --- |
| API/registration access | Host adapters and admin tooling | HAProxy candidate bound to `127.0.0.1:7443`, with three inventory-generated API backends on `6443`, TCP passthrough and backend health checks |
| API name resolution | DNS owner and host adapters | Stable logical API name; local proxy clients resolve it to loopback independently of cluster DNS; no Cloudflare proxy |
| Server certificates | Host adapter/K3s | Include the logical API name in every server's TLS SANs; joining nodes use that name; admin helpers preserve hostname and CA verification |
| Private reachability | Host networking | All discovered API backends reachable; if private control-plane IPs are advertised, two always-on subnet routers advertise identical narrow routes and remote clients accept them |
| Initial server | Host adapter | Initialize the first server directly; later servers and agents use their host-managed proxy |
| Proxy lifecycle | Host adapter/admin helper | Start outside Kubernetes before enrollment or admin access; reserve `6444` for K3s |
| Admin authorization | Tailscale policy and Kubernetes RBAC | Explicit API access and tested admin connection helper |
| Emergency access | Host adapter | Direct host Tailscale/SSH access and provider recovery access |
| Bootstrap | Host adapter plus administrative tooling | Reach Git, images, secrets and backups without Flux, cluster DNS, cache or dashboard |
| Flux ownership handover | Bootstrap procedure | Install the root reconciliation once; avoid competing manifest owners |

[K3s agents discover API backends](https://docs.k3s.io/architecture) after registration; this does not make the initial seed address or `kubectl` endpoint highly available. The local placement of [HAProxy TCP load balancing](https://docs.k3s.io/datastore/cluster-loadbalancer) is a candidate: verify cold enrollment with one server down, discovered endpoint reachability, TLS verification and proxy/router failure before adoption. [Tailscale route failover](https://tailscale.com/docs/how-to/set-up-high-availability) requires matching prefixes and is not instantaneous. DNS round-robin or a cross-provider floating IP is not an assumed solution; a provider TCP load balancer remains an optional adapter.

## Portable host and network contract

| Contract | Requirement |
| --- | --- |
| Host inventory | Stable ID, provider/location, architecture, OS adapter, addresses, storage and measured capabilities |
| OS baseline | Supported NixOS or supported Ubuntu/Debian; pinned K3s version and architecture-compatible components |
| Control-plane mode | Normal `k3s server` with embedded etcd, kubelet and containerd; `NoSchedule` taints exclude application/CI pods; only approved system pods receive tolerations |
| Control-plane flags | Keep etcd enabled; no experimental `--disable-agent` mode |
| Host prerequisites | Compatible kernel, cgroup controllers, containerd/cgroup driver, forwarding, TUN/CNI modules, time sync, DNS and explicit firewall rules verified |
| Disk prerequisites | Persistent capacity, filesystem/snapshotter compatibility, I/O contention and available recovery space verified |
| Node admission | Platform validation sets eligibility labels under `node-restriction.kubernetes.io/`; verify Node authorizer, NodeRestriction admission and RBAC prevent node/workload self-authorization |
| Eligibility renewal | Withdraw eligibility and drain affected work before OS/kernel/runtime changes; repeat compatibility checks before restoring eligibility |
| Cross-provider networking | Host-owned encrypted transport independent of Kubernetes; keep etcd on the colocated private network |
| Network pilot | Select and test pod CNI, advertised node/API addresses, non-overlapping CIDRs, private API routes, NAT/CGNAT behavior and MTU together |
| Tailscale integration | Existing host Tailscale is an adapter; do not assume experimental K3s `--vpn-auth` is production-ready |
| Add machine | Inventory entry, provider provision/import or SSH enrollment, host configuration, capability checks, then join |
| Remove machine | Cordon/drain, move or restore local data, verify replicas/backups, delete the Kubernetes node and revoke host/network credentials before removing the provider resource |
| OS ownership | One adapter owns each host setting; NixOS generation rollback has no automatic Ubuntu/Debian equivalent |
| Portability exercise | Rebuild an eligible worker and restore an application on another provider and supported OS |
| External integrations | DNS/Cloudflare, Tailscale, GHCR, Doppler and object storage have explicit credentials and replacement procedures |

[K3s requirements](https://docs.k3s.io/installation/requirements) cover modern Linux and both x86_64/ARM64. Provider independence does not require identical installation mechanisms; project configuration must not encode provider names, host paths or cloud-specific volume identifiers.

[Ansible](https://docs.ansible.com/projects/ansible/latest/installation_guide/intro_installation.html) requires SSH/Python on managed Linux hosts. [Kubernetes runtime prerequisites](https://kubernetes.io/docs/setup/production-environment/container-runtimes/) cover forwarding and matching cgroup drivers; [OverlayFS](https://docs.kernel.org/filesystems/overlayfs.html) requires suitable extended attributes and directory entry support. Validate the selected versions rather than treating a provider's Docker image as proof of compatibility.

[NodeRestriction-protected labels](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#node-isolationrestriction) prevent kubelets claiming eligibility; label changes alone do not evict running pods. [K3s agentless mode](https://docs.k3s.io/advanced#running-agentless-servers-experimental) is experimental and removes normal node registration; dedicated control-plane placement instead retains the standard server runtime with scheduling restrictions.

## Ownership and project contract

| Resource | Sole declarative owner | Project input |
| --- | --- | --- |
| Provider resources and DNS records | OpenTofu | Approved domain request |
| OS, disks, firewall and K3s version | Selected host adapter | None |
| Namespaces, RBAC, quotas and admission | Platform Flux reconciliation | Approved workload class |
| Cluster routes and ingress objects | Flux | Domain reference from approved catalog |
| Application deployment | Namespace-scoped Flux reconciliation | Immutable release digest and validated workload settings |
| Image build and application tests | Application repository through shared workflows | Dockerfile/build definition and project test commands |
| Image promotion | Trusted release workflow | Project/environment identity, digest and verified signature/provenance |
| Data lifecycle and backups | Infrastructure project declaration | Database, volume, retention and recovery requirements |
| Onboarding | CLI-generated reviewable change | Repository, workload class, port, health path and requested capabilities |

| Contract rule | Required behavior |
| --- | --- |
| Repository identity | Derived from authenticated caller; never trusted solely from submitted YAML |
| Domain identity | Approved catalog prevents one project claiming another project's hostname |
| Configuration | Reject arbitrary `extraObjects`, `tpl`, pod specifications, host paths, RBAC and privilege settings |
| Namespace | Project cannot select another namespace or change its reconciliation service account |
| Isolation | Namespace-scoped service accounts, Pod Security/admission controls, quotas and default-deny traffic |
| Exceptions | Explicit platform-owned extensions with the same policy gates |
| CLI effect | Generate/validate configuration; submitting a request does not grant infrastructure privileges |
| Promotion result | Record requested digest, health result and deployed revision per environment |

A schema validates shape; authentication and admission establish authority.

| Domain requirement | Value |
| --- | --- |
| Portfolio | `hansteen.dev` |
| Planned personal webpage | `fredrir.com`, reserved for `fredrir/fredrir` |
| Operations | Proposed authenticated `grafana.fredrir.com` and `admin.fredrir.com` |
| Logs shortcut | `fredrir.com/logs` may redirect to Grafana without moving the portfolio |
| Unassigned domain | `fredrir.no`; infrastructure owns DNS while purpose remains unassigned |

## CI and cache capacity

[ARC](https://docs.github.com/en/actions/concepts/runners/actions-runner-controller) supports repository-scoped runner scale sets. Sharing production workers accepts a common node/cluster failure domain; namespaces alone do not establish a separate kernel boundary. [Kubernetes multi-tenancy](https://kubernetes.io/docs/concepts/security/multi-tenancy/)

| Name | Requirement |
| --- | --- |
| Scope | One registration boundary per personal repository; no assumed organization-wide runner pool |
| Eligibility | CI for maintainer-approved revisions; approval authorizes execution but does not establish benign code or dependencies |
| CI capacity | Per-worker `infra.fredrir.com/ci-slot`: `fredrir-04` 1, `fredrir-09` 3; one slot per job |
| Per-repository maximum | Local scale-set ceiling; does not establish a fleet-wide concurrency cap |
| Aggregate enforcement | Measured resource classes, quotas and production reservations account for runner, job, build and service pods; quotas alone do not establish a global build count |
| ARC execution mode | Required job containers; narrowly scoped controller/runner pod-management privileges never exposed to build containers |
| Queue behavior | Enable multiple repositories only after tests prove the aggregate build budget, backpressure and eventual progress across all runner, job, build and service pods |
| Shared workflows | Explicit inputs and project test hooks; publication authority scoped separately from build execution |
| Untrusted public/fork PRs | No workflow job execution: job-level guards and pool job-started hooks refuse fork heads |
| GitHub repository setting | Require approval for all external contributors and never approve their PR runs |
| Public repository enablement | Contents/history audit and no-external-PR protections pass before any visibility change or runner enrollment |
| Event boundary | Protected `main` pushes; same-repository PRs on a read-only-cache pool; protected `v*` tags on the release pool |
| Pre-allocation checks | Every executable job checks authenticated repository/owner identity, event, exact allowed ref and protection with job-level conditions |
| Reusable workflow boundary | Caller runner labels cannot authorize execution; each pool's job-started hook checks event, ref and protection |
| Integration and release | Maintainer review precedes integration; successful post-integration tests precede publication/deployment |
| Merge requirements | Do not require PR checks that this policy never runs |
| Hosted runners | No GitHub-hosted execution or recovery fallback |
| CI recovery | Admin tooling rebuilds the runtime/controllers independently of existing runners |
| Build secrets | No production kubeconfig, host deploy key, controller token or infrastructure cache signing key |
| K3s worker enrollment | Separate K3s agent token or expiring bootstrap token; workers never receive the server/recovery token |
| Registry pull credentials | Separate scoped GHCR read credentials for runtime; OctoSTS release authorization is not registry authentication |
| Cache trust | Separate infrastructure writes from project writes; an upload token can authorize signing even when the private signing key stays off-runner |
| Cache retention | Scoped quotas and garbage collection preserve necessary recovery artifacts |
| Recovery | Public/bootstrap sources and retained artifacts remain usable without the self-hosted cache |

[GitHub's external-contributor approval setting](https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/enabling-features-for-your-repository/managing-github-actions-settings-for-a-repository) also matters when a fork changes workflow files; deleting current PR triggers alone is insufficient. Job-level conditions run before runner dispatch. [GitHub contexts](https://docs.github.com/en/actions/reference/workflows-and-actions/contexts)

| Sandbox gate | Required result |
| --- | --- |
| Configured runtime | gVisor `runsc` with `systrap`; the pinned Nix pairing fails ordinary sandboxed builds on amd64 |
| Alternative under review | Kata with a standard Linux guest on independently verified KVM-capable workers; no runtime or production selection |
| Runtime installation | Host adapter explicitly configures containerd shim/template; do not assume K3s auto-detects `runsc` |
| Runtime enforcement | Platform admission enforces RuntimeClass on runner-created job, action, build and service pods |
| Workload restrictions | No host paths, host namespaces, host Docker/containerd sockets or host-privileged build pods |
| Nix | Real project builds with `sandbox=true` and `sandbox-fallback=false`; no production Nix daemon/store mount |
| Container builds | Real Dockerfile/action/BuildKit compatibility; no automatic seccomp/AppArmor relaxation |
| VM tests | KVM is a separate capability; neither gVisor nor Hetzner Cloud guarantees NixOS VM-test support |
| Multiarch release | Test each image variant and its runner/actions/sidecars before publishing a pinned OCI index |
| Resources | Bound CPU, memory, ephemeral storage, PIDs and build concurrency; prove production survives combined I/O load |
| Fallback | Unsupported builds remain blocked until an explicit shared-kernel risk decision or separate execution capacity is approved |

Priorities and CPU quotas do not bound disk I/O; combined production/build load must meet the workload's latency and recovery limits before shared CI is enabled.

[gVisor platforms](https://gvisor.dev/docs/user_guide/platforms/) and [architecture support](https://gvisor.dev/docs/user_guide/faq/) establish a candidate, not verified ARC/Nix compatibility. [K3s runtime configuration](https://docs.k3s.io/advanced) needs explicit integration. Documented ARC DinD requires privileged pods even in its rootless variant; upstream rootless BuildKit examples also relax container security settings. [ARC deployment](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/deploy-runner-scale-sets), [BuildKit limitations](https://github.com/moby/buildkit/blob/master/docs/rootless.md)

The [CI runtime pilot](ci-runtime-pilot.md) records the observed gVisor failures and fredrir-09 KVM instruction test. Neither result establishes ARC/K3s compatibility or native ARM support.

[K3s tokens](https://docs.k3s.io/cli/token) default agent enrollment to the server token unless separated. Nix's default sandbox fallback must be disabled for the pilot. [Nix configuration](https://nix.dev/manual/nix/2.35/command-ref/conf-file.html)

## Data and ingress migration

| Stage | Required behavior |
| --- | --- |
| Preserve identities | Retain cloud resource addresses, database names, volume identities and UID mappings until an explicit migration |
| Stateless pilot | Deploy a small application using the shared contract before migrating databases |
| Temporary database access | Restricted connection to existing host-managed PostgreSQL/Valkey |
| Volume policy | Explicit storage class, retention, backup and deletion protections |
| Database rehearsal | Restore into an isolated target; verify data, extensions, roles and application behavior |
| Cutover | Stop writes, complete transfer, verify target, switch endpoints and preserve the former instance offline |
| Rollback boundary | Before target writes, switch back; after target writes, reconcile data before any reversal |
| Image rollback | Revert the approved digest; database schema must remain compatible |
| Ingress coexistence | Disable packaged Traefik/ServiceLB on every K3s server; Flux Traefik uses ClusterIP while host Caddy owns ports 80/443 |
| Edge cutover | Transfer route/port ownership once; verify public routes, private admin auth and health checks |
| Removal | Remove former workload reconciliation only after data retention and recovery checks pass |

K3s local-path volumes remain bound to their storage node; the [shipped StorageClass](https://github.com/k3s-io/k3s/blob/main/manifests/local-storage.yaml) defaults to `Delete`. Valuable data requires explicit protection and independent backups. [K3s storage](https://docs.k3s.io/add-ons/storage)

Default Traefik/ServiceLB uses host ports 80/443. Coexistence with current Caddy needs explicit configuration before enabling K3s. [K3s networking services](https://docs.k3s.io/networking/networking-services)

## Recovery

| Artifact | Backup/recovery requirement |
| --- | --- |
| OpenTofu state | Versioned, locked remote state outside the cluster; reviewed address/backend migrations |
| Cluster datastore | Scheduled native etcd snapshots encrypted and copied off-host by backup tooling; matching K3s server token in protected recovery escrow |
| PostgreSQL/Valkey/files | Application-consistent native backups inside encrypted Restic repositories; etcd snapshots contain no volume data |
| Backup destinations | Independent account/failure domain; separate off-site copy and credentials that remain available without Kubernetes |
| Backup transport | Interchangeable Restic S3-compatible, SFTP or HTTPS REST endpoint; verify actual backend capabilities |
| Deletion protection | Backend-enforced append-only/immutability where supported; separate backup and prune credentials |
| Retention administration | Protected maintenance credentials; append-only retention uses time windows that cannot be displaced by forged snapshot counts |
| Cross-architecture PostgreSQL | Logical dump/restore with roles and compatible extensions; custom-format dumps use `pg_restore`; physical recovery requires a validated matching architecture/version |
| Provider snapshots | Optional extra recovery mechanism; no dependency for canonical backups or restore |
| Stateful partition | Fence or verify shutdown of the former writer before restoring/promoting a replacement |
| K3s upgrade | Retain the prior package and pre-upgrade datastore snapshot; upgrade one server at a time |
| NixOS generation | Restores host configuration; does not undo database or Kubernetes datastore changes |
| Cluster restore | Recover onto another provider; restore token, datastore and application data, then reconcile old node/storage/provider identities |
| Credentials | Recovery material accessible without the failed cluster, production cache or dashboard |

The server token is required to decrypt restored K3s datastore contents. Minor-version rollback requires an appropriate datastore backup together with the prior K3s binary. [K3s backups](https://docs.k3s.io/datastore/backup-restore), [K3s rollback](https://docs.k3s.io/upgrades/roll-back)

[Restic](https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html) supports multiple storage backends; encryption alone does not prevent deletion. [Append-only protection](https://restic.readthedocs.io/en/stable/060_forget.html#security-considerations-in-append-only-mode) depends on backend enforcement, protected pruning and time-window retention. [PostgreSQL SQL dumps](https://www.postgresql.org/docs/current/backup-dump.html) support cross-machine architecture migration; [physical WAL shipping](https://www.postgresql.org/docs/17/warm-standby.html) requires matching hardware architecture and major version.

[K3s snapshot restore](https://docs.k3s.io/cli/etcd-snapshot) supports new hosts with the original token; S3 recovery credentials cannot come solely from a Kubernetes Secret because the API is unavailable during restore. A disconnected StatefulSet process may still be running; automatic force deletion risks a second writer. [StatefulSet deletion](https://kubernetes.io/docs/tasks/run-application/force-delete-stateful-set-pod/)

## Phase gates

| Phase | Exit gate |
| --- | --- |
| 1. Inventory and design | Provider terms/specs, live identities, use, ownership and RPO/RTO reviewed; urgent NixOS/Tailscale fixes verified; contents/history audit and no-external-PR protections pass before any visibility change |
| 2. Compatibility pilot | Admin tooling bootstraps a disposable pilot cluster; supported OS, cross-provider network, ARM/x86 build sandbox, I/O limits and enrollment credentials verified |
| 3a. CPX22 evacuation | Select measured, compatible temporary capacity on existing CCX23 or new workers; restore verification and single-writer cutover pass for CPX22 applications/data before conversion |
| 3b. Host foundation | CX33 pair and evacuated CPX22 form quorum; API/bootstrap failure cases pass |
| 4. Bootstrap and policy | Independent recovery path, Flux ownership and project admission boundaries verified |
| 5. CI and onboarding | No external PR execution; CLI, shared workflows, artifact promotion, isolation and cross-repository capacity tests pass |
| 6. Remaining stateless migration | Digest deployment, health checks, route cutover and rollback pass |
| 7. Remaining stateful migration | Restore rehearsal and single-writer cutover pass for each data service |
| 8. Shared services | Cache, logs, metrics, traces and alerts stay within measured retention/resource budgets |
| 9. Resilience | Control-plane and worker failures measured separately; cross-provider restore meets RPO/RTO |
| 10. Home workers | Network interruption leaves production healthy; eligible jobs recover without duplicate effects |

## OpenTofu validation context

| Name | Value |
| --- | --- |
| Observed local runtime | OpenTofu `1.12.6` |
| Locked providers | hcloud `1.68.0`; AWS `5.100.0`; Cloudflare `5.23.0` |
| State backend | S3 `llunde-pyparser-bucket`, key `tofu-state/infra.tfstate` |
| Execution | Local research only; proposed local planning and CI validation; infrastructure treated as production |
| Risks | Identity churn, secret exposure, blast radius and state corruption |
| Migration | Preserve addresses initially; explicit `moved` blocks for address refactors |
| Approval artifact | Protected saved plan; review before applying that exact artifact |
| Recovery evidence | Protected prior state versions and migration records; reconcile live resources before state restoration |

| Check | Proposed command |
| --- | --- |
| Format | `tofu -chdir=tofu fmt -check -recursive` |
| Isolated provider initialization | `tofu -chdir=/path/to/isolated-checkout/tofu init -backend=false -lockfile=readonly` |
| Isolated validation | `tofu -chdir=/path/to/isolated-checkout/tofu validate` |
| Credentialed review plan | `tofu -chdir=tofu plan -out=/protected/path/infra.tfplan` |

## Behavioral acceptance

| Scenario | Pass condition |
| --- | --- |
| New project | CLI produces valid configuration and shared workflow; first healthy deployment needs no copied platform logic |
| Unauthorized project input | Foreign domain/namespace, arbitrary resources and privileged pod settings are rejected |
| External PR, including workflow changes | No job execution or runner allocation for first-time or returning external contributors |
| Unauthorized event or reusable-workflow caller | Rejected before runner allocation; no comment/dispatch/rerun bypass |
| Approved CI build | Cannot reach production data/hosts, read other project secrets or publish trusted infrastructure cache entries |
| Sandbox missing or incompatible | Job remains blocked; no fallback to ordinary or privileged host execution |
| Concurrent repositories | Running builds remain within the global budget; queued work progresses after capacity frees |
| Bad release | Health gate fails; deployed digest and rollback outcome are observable |
| Single control-plane failure | API access and cold worker enrollment succeed against surviving servers |
| Shared worker failure | Production fits surviving eligible nodes where storage permits; CI queues within architecture-specific capacity |
| API route/proxy failure | Admin helper and new-agent bootstrap reach healthy API backends through remaining routes |
| Node-local volume loss | No false healthy deployment; restore is measured against the declared RPO/RTO |
| Cross-provider restore | Datastore/token and application restore succeed on replacement capacity without cache/registry/dashboard circular dependencies |
| WAN partition | No replacement database writer before fencing; retries do not duplicate external effects |
| K3s version rollback | Matching package and datastore recover; application data remains independently accounted for |
| Home disconnection | No quorum or critical service loss; eligible jobs retry safely |
| Observability | A failed deployment, missed backup and production-site outage produce actionable signals |
| Retention pressure | Image, cache, log and trace growth remains bounded without deleting protected recovery data |

| Validation status | Value |
| --- | --- |
| Research and source inspection | Completed |
| Infrastructure implementation | Not performed |
| Runtime/failure tests | Not executed |
| Procurement | Not performed |
