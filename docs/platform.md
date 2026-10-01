# Shared platform

[Go commands, build architecture and binary distribution](development.md)

| Name                       | Value                                                                                                                                                                                                                   |
| -------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Repository                 | `fredrir/infra`                                                                                                                                                                                                         |
| Inventory                  | [Ansible production inventory](../ansible/inventory/production.yml)                                                                                                                                                     |
| Hosts                      | Ubuntu 26.04 LTS; [node identities and roles](nodes.md)                                                                                                                                                                 |
| Cluster                    | K3s; three embedded-etcd servers and two shared workers                                                                                                                                                                 |
| Host configuration         | Ansible, native K3s `config.yaml`, systemd and nftables                                                                                                                                                                 |
| Cluster configuration      | Flux, HelmRelease, Kustomize and ordinary YAML                                                                                                                                                                          |
| Infrastructure credentials | `secrets/operator.sops.yaml`, Macie and Archie only                                                                                                                                                                                                   |
| Runtime secrets            | SOPS + age; separate Macie, Archie and Flux recipients                                                                                                                                                                  |
| Scheduling                 | Kubernetes requests, limits, quotas, priorities, protected runtime capability labels and per-worker CI slots                                                                                                            |
| Images                     | GHCR, immutable digests                                                                                                                                                                                                 |
| Version pins               | [Platform versions](../platform/versions.yaml), role defaults, image Dockerfiles, `MODULE.bazel.lock`; runner image digests in the runner sets and the [admission policy](../platform/components/policy/admission.yaml) |

## Ownership

| Owner                    | Resources                                                                            |
| ------------------------ | ------------------------------------------------------------------------------------ |
| OpenTofu                 | Provider machines, private network, DNS, tunnels and restricted backup/mail IAM      |
| Ansible                  | Host packages, SSH, firewall, Tailscale, K3s, containerd and native services         |
| Flux platform reconciler | Shared services, project namespaces, network policy, admission and encrypted secrets |
| Flux project reconcilers | Four project owners consume content-addressed artifacts; policy readiness and parser migration ordering remain required |
| Source watcher | [Selective reconciliation and generation](flux-artifacts.md) |
| Project repository       | Source, native tests, Dockerfiles and a pinned shared workflow caller                |
| Deployment workflow            | Verified image digest and source revision, limited to the configured application     |
| Application owner        | Database schema compatibility and recovery requirements                              |

Provider APIs provision machines; an existing SSH-accessible machine enters through Ansible inventory. Workers use encrypted Tailscale connectivity across providers. Etcd stays on the three colocated control servers. Macie and Archie are administration and recovery machines.

## Services

| Address                                  | Service                                                               |
| ---------------------------------------- | --------------------------------------------------------------------- |
| `fredrir.com`                            | Reserved for the future `fredrir/fredrir` application                 |
| `grafana.fredrir.com`                    | Authenticated Grafana, Prometheus metrics, Loki logs and Tempo traces |
| `cache.fredrir.com`                      | Retired Attic endpoint; provider resources retained                   |
| `pkgs.fredrir.com`                       | Signed apt, rpm and apk repositories and `install.sh`                 |
| `admin.fredrir.com`                      | Reserved for the future dashboard                                     |
| `fredrir.no`                             | Centrally managed DNS; purpose unassigned                             |
| `hansteen.dev`                           | Portfolio                                                             |
| `yeeter.no`                              | Y                                                                     |
| `parser.llunde.no`, `external.llunde.no` | Parser review and shared media                                        |
| `llunde.no`, `api.llunde.no`             | Work-in-progress Llunde frontend/backend                              |

| Shared component       | Configuration                                                                                                                                                                                                                                                                     |
| ---------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Ingress                | Two Traefik instances and two Cloudflare Tunnel instances; project ingress class `platform`                                                                                                                                                                                       |
| Metrics                | Prometheus; 15 days or 18 GB                                                                                                                                                                                                                                                      |
| Logs                   | Alloy → Loki; 7 days                                                                                                                                                                                                                                                              |
| Traces                 | Alloy → Tempo; 7 days, bounded ingestion and sensitive attribute removal                                                                                                                                                                                                          |
| Cluster DNS            | NodeLocal DNSCache on every node; answers on `10.43.0.10` and `169.254.20.10`; cluster zones → CoreDNS over TCP, other names → node resolvers; host firewall admits pod DNS on `cni0`; project pods use `ndots:2` |
| Nix cache              | Attic scaled to zero; retained PVC, signing identity and hourly backups                                                                                                                                                                                                           |
| Build cache            | Object store `seaweedfs-hel1`: `ci-<project>-main` 20 GiB (sccache, `target/` archive), `ci-<project>-release` 10 GiB, 14-day expiry; `toolchains` 5 GiB; read-only, read-write and release credentials per project |
| Object store           | SeaweedFS cells `seaweedfs-hel1` (`fredrir-04`) and `seaweedfs-nl` (`fredrir-09`); S3 over TLS only; [operation](runbook.md#object-store) |
| Bazel cache            | bazel-remote on `fredrir-09`, tailnet only: read-only gRPC on 9092, writes on 9093; [operation](runbook.md#bazel-cache) |
| Independent monitoring | Gatus on `fredrir-06`; 13 endpoint checks including `production` ⊆ `main`, five authenticated backup heartbeats and the `reconciliation` deep, verification and apply heartbeats; cluster alert when unreachable                                                                                                                                    |
| Email                  | `alerts@fredrir.com`; [credentials and operation](mail-alerts.md)                                                                                                                                                                                                                 |

## CI and deployments

| Name                  | Value                                                                                                                                                                                                                                                                                          |
| --------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Runner registration   | Separate `fredrir-infra-runners` GitHub App; encrypted credentials in ARC namespaces                                                                                                                                                                                                           |
| Container builds      | Dagger through the compiled Go CLI on qualified `infra-build-09`; GitHub-hosted runners provide bootstrap; Bazel and Dagger keep separate caches; `infra pipeline image --target <target> --check-only` validates an intermediate stage                                                          |
| Nix builds            | Retired; infrastructure workflows use Bazel and the pinned Go SDK                                                                                                                                                                                                                              |
| Rust builds           | Ephemeral gVisor pools on the [Rust runner image](../images/runner-rust/Containerfile): `rust-pr-amd64`, `rust-amd64`, `rust-release-amd64`; `rust-amd64` and `rust-pr-amd64` prefer fredrir-04 and never run on volatile workers; sccache on the build cache; `target/` archive per toolchain, written by `rust-amd64` only                                                         |
| CI capacity           | Check, deploy and Rust ARC pools retain slot limits; hosted Dagger jobs do not consume cluster slots; [execution boundaries](development.md#execution-boundaries)                                                                                                                              |
| JavaScript actions | Node 24 LTS; pinned native Node 24 actions; explicit workflow runtime selection |
| Runner permissions    | Cluster ARC pools have no host sockets, host paths or Kubernetes API token; Dagger runs on isolated hosted machines or qualified dedicated VMs                                                                                                                                                 |
| Build egress          | Hosted build network; cluster Rust pools retain DNS, HTTPS and object store TCP 8333                                                                                                                                                     |
| Repository checks     | [`check.yml`](../.github/workflows/check.yml): verified release reuse with hosted Bazel fallback, native Go tests, generated BUILD drift checks, changed infrastructure validation, and a real SSH hop for reconciler host access                                                                                                                                    |
| S3 filter test        | [`s3-filter.yml`](../.github/workflows/s3-filter.yml) on changes to the filter, its test or `images/caddy`: the shipped `s3-filter.caddyfile` against allowed and bypass requests, from its own Go module `platform/components/object-store/filtertest` on the `images/caddy` pins; no cache or deploy credentials; `filtertest/s3-filter.caddyfile.sha256` pins the Caddyfile in the contract tests, so every filter edit touches `filtertest/` and runs this check |
| Alert rule test       | `check.yml` job `alerts` on changes to the object store component, `platform/versions.yaml` or the cases: `promtool` from the pinned Prometheus image runs `internal/policy/testdata/object-store-alerts.yaml` against the object store rules |
| Job timeout           | 45 minutes; callers with long native test suites pass `timeout-minutes`                                                                                                                                                                                                                        |
| Runner scratch        | Retained cluster pools enforce scratch storage limits; dedicated VM cache collection and resource ceilings are declared in the build-engine role                                                                                                                                               |
| Shared images         | [`images/catalog.yaml`](../images/catalog.yaml): Rust and check runner images, backup tools and Caddy; [`images.yml`](../.github/workflows/images.yml) selects changed declared inputs and injected `infra` binary digests, scans and attests before setting content tags; Monday schedule and manual dispatch rebuild all |
| Fork PRs              | Never run: job-level guard, pool job-started hook and approval required for every external contributor                                                                                                                                                                                         |
| Approved source       | Protected `main` pushes; same-repository PRs on `rust-pr-amd64`; protected `v*` tags and `main` dry runs on `rust-release-amd64`; matching numeric repository and owner identities                                                                                                             |
| Workflow reuse        | `fredrir/infra/.github/workflows/{project-ci,project-images,rust-auto-tag,rust-release,packages-publish}.yml@ci-v1`                                                                                                                                                                     |
| Pin changes           | Qualified immutable CI releases advance `ci-v1` after the central deployment trust update is merged                                                                                                                                                                             |
| Release authorization | OctoSTS on `infra`, onboarded projects, `packages`, `homebrew-tap`, `homebrew-nsql` and `nur-packages`; policies pin workflow path, repository id, ref and environment; project deploy identities hold `actions: write` on `infra`, only `deploy.yml` on `main` holds `contents: write`        |
| Deployment mappings   | [Repository image mappings](../.github/deployments)                                                                                                                                                                                                                                            |
| Promotion             | `build-image.yml` dispatches [`deploy.yml`](../.github/workflows/deploy.yml); it verifies the GitHub attestation (public) or keyless Cosign signature (private) against an approved exact workflow revision, pushes the digest to `main` and notifies the Flux `deploy` receiver                        |
| Rollback              | Revert the deployment commit; check database schema compatibility first                                                                                                                                                                                                                        |
| Native ARM            | Rust targets cross-compile with cargo-zigbuild; container images and native tests stay amd64 until an ARM worker passes qualification                                                                                                                                                          |

Project callers declare triggers, permissions and the shared workflow. [Project profiles](../build/projects) own checks, affected inputs and image matrices.

```yaml
name: Build
on:
  push:
    branches: [main]
permissions:
  contents: read
  packages: write
  id-token: write
  attestations: write
jobs:
  build:
    uses: fredrir/infra/.github/workflows/project-images.yml@ci-v1
```

## Rust projects

| Name           | Value                                                                                                                                                          |
| -------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Registry       | [Rust projects](../.github/rust-projects.yaml)                                                                                                                 |
| CI             | [`project-ci.yml`](../.github/workflows/project-ci.yml): central checks; Rust formatting, lint, tests and minimal builds; scheduled MSRV and audit                                           |
| Versioning     | [`rust-auto-tag.yml`](../.github/workflows/rust-auto-tag.yml): patch bump, git-cliff, `v*` tag                                                                 |
| Release        | [`rust-release.yml`](../.github/workflows/rust-release.yml): GoReleaser OSS and cargo-zigbuild                                                                 |
| Targets        | `x86_64`/`aarch64` Linux gnu (glibc 2.28) and musl (static); `x86_64`/`aarch64` macOS (SDK in `toolchains`)                                                    |
| Channels       | GitHub Release, crates.io (trusted publishing), `fredrir/homebrew-tap`, AUR `<name>` and `<name>-bin`, `fredrir/nur-packages`, `pkgs.fredrir.com`              |
| Publisher      | `fredrir/packages` → [`packages-publish.yml`](../.github/workflows/packages-publish.yml); dispatch after each release, daily reconcile, weekly full smoke test |
| Package trust  | Attested by the release workflow on an infra `main` revision; latest 3 stable releases per project                                                             |
| Public keys    | [GPG](../internal/packages/assets/fredrir.asc), [apk](../internal/packages/assets/fredrir.rsa.pub)                                                             |
| Private repos  | CI only                                                                                                                                                        |
| Release config | `[package.metadata.release]`: `maintainer`, `section`, `features`, `extra-files`, `optional`, `depends`                                                        |

```sh
infra onboard-rust fredrir/example \
  --project example \
  --output .infra/onboarding/example
```

| Generated                                   | Destination                                     |
| ------------------------------------------- | ----------------------------------------------- |
| `platform/components/runners/example/`      | Written in place; three pools and cache secrets |
| `platform/components/object-store/`         | Written in place; cache credentials, identities and buckets |
| `build/projects/example.json`, `.github/rust-projects.yaml` | Written in place                                |
| `project/.github/`                          | Project repository callers and auto-tag policy  |
| `packages/.github/chainguard/`              | `fredrir/packages`                              |

| Project setting       | Value                                                              |
| --------------------- | ------------------------------------------------------------------ |
| Rulesets              | `main` and `v*` tags protected; bypass: repository admin, Octo STS |
| Environment `release` | Tags `v*`                                                          |
| Actions               | Approval required for all external contributors                    |
| Releases              | Immutable                                                          |
| crates.io             | Trusted publisher: workflow `release.yml`, environment `release`   |

```sh
gh workflow run release.yml --repo fredrir/example
gh workflow run publish.yml --repo fredrir/packages
infra operations macos-sdk package .infra/macos-sdk
SOPS_AGE_KEY_FILE="$HOME/.config/age/keys.txt" infra operations macos-sdk upload .infra/macos-sdk/MacOSX<version>.sdk.tar.zst
```

## Onboarding

```sh
infra onboard fredrir/example \
  --project example \
  --image "ghcr.io/fredrir/example@sha256:$image_digest" \
  --source-revision "$source_revision" \
  --workflow-ref "$workflow_revision" \
  --domain example.fredrir.com \
  --test-command '<native-project-test-command>' \
  --output .infra/onboarding/example
```

| Generated files                       | Destination                                                                                |
| ------------------------------------- | ------------------------------------------------------------------------------------------ |
| `project/`                            | `platform/projects/example/`; include it in the projects Kustomization and regenerate selective Flux artifacts                     |
| `infrastructure/build/projects/`      | Shared CI profile                                                                          |
| `infrastructure/.github/`             | Infrastructure deployment mapping (`visibility`, image paths) and OctoSTS trust policy     |
| `caller/.github/workflows/build.yaml` | Project repository                                                                         |
| Application credentials               | Add encrypted `project-registry` and `project-runtime` Secrets                             |
| Activation                            | Review generated YAML, resource limits and secrets, then raise workload replicas from zero |

Container onboarding uses the qualified VM when enabled and hosted Dagger jobs otherwise; Rust onboarding generates its scoped object store cache credentials, identities and buckets.

| `infra` setting  | Value                                                                                   |
| ---------------- | --------------------------------------------------------------------------------------- |
| `main` ruleset   | [`main-ruleset.json`](../.github/main-ruleset.json); bypass: repository admin, Octo STS |
| `production` rulesets | [`production-ruleset.json`](../.github/production-ruleset.json), bypass: publisher App; [`production-history-ruleset.json`](../.github/production-history-ruleset.json), no bypass; [publishing](runbook.md#publishing) |
| Private packages | Package settings → Manage Actions access: `fredrir/infra`, read                         |

The shared [project chart](../charts/project) supports web services, workers and scheduled jobs. Databases and durable files require explicit storage, backup and recovery configuration.

## Host operation

```sh
export ANSIBLE_CONFIG=ansible/ansible.cfg
export SOPS_AGE_KEY_FILE="$HOME/.config/age/keys.txt"
uv run --frozen --group ci ansible-playbook ansible/site.yml --limit fredrir-NN
uv run --frozen --group ci ansible-playbook ansible/k3s.yml --limit fredrir-NN -e k3s_registration_server=fredrir-08
uv run --frozen --group ci ansible-playbook ansible/ci-runtimes.yml --limit fredrir-NN
uv run --frozen --group ci ansible-playbook ansible/volatile.yml --limit fredrir-10
uv run --frozen --group ci ansible-playbook ansible/maintenance.yml
uv run --frozen --group ci ansible-playbook ansible/external.yml --limit fredrir-06
```

| Prerequisite           | Value                                                                                                                      |
| ---------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| SSH                    | Verified host key, administrator public keys, root or passwordless sudo                                                    |
| Transport              | Enrolled Tailscale role; native OpenSSH, embedded Tailscale SSH disabled                                                   |
| New-node enrollment    | `infra operations enrollment`; verified `infra` binary on target; [bootstrap playbook](../ansible/tailscale-bootstrap.yml) |
| Enrollment credentials | `secrets/operator.sops.yaml` `TAILSCALE_ENROLL_CLIENT_ID` and `TAILSCALE_ENROLL_CLIENT_SECRET`; owner tag `tag:platform-enrollment`   |
| K3s credentials        | Separate server/agent tokens in private `/etc/rancher/k3s/` files                                                          |
| Registration seed      | Healthy, already clustered control server; default fresh-cluster initializer `fredrir-07`                                  |
| Control network        | `10.60.0.5`, `10.60.0.7`, `10.60.0.8`; etcd stays private                                                                  |
| Admin API              | Verified kubeconfig; API port 6443 through approved narrow subnet routes                                                   |
| Maintenance            | Serial upgrades; reboot only when required; verify new boot identity, API and NodeReady before proceeding                  |

```sh
export KUBECONFIG=/path/to/private/kubeconfig
kubectl get nodes -o wide
flux get all --all-namespaces
kubectl get pods --all-namespaces
kubectl get cronjobs --all-namespaces
```

## Backups and recovery

| Data | Frequency | Destination | Retention |
| --- | --- | --- | --- |
| Parser PostgreSQL, local files and dataset | Daily, 00:15 UTC | SeaweedFS `hel1` | Latest 3 snapshots |
| Y MongoDB and media | Daily, 03:35 UTC | SeaweedFS `hel1` | Latest 3 snapshots |
| Portfolio PostgreSQL | Daily, 04:05 UTC | SeaweedFS `hel1` | Latest 3 snapshots |
| Parser, Y and portfolio | Every 14 UTC calendar days since the last successful snapshot | AWS S3 | Latest snapshot |
| Attic SQLite, including cache signing identity | Hourly, :45 UTC | SeaweedFS `hel1` | 7 daily, 4 weekly, 12 monthly |
| Control-plane recovery data | Daily, 05:55 UTC | AWS S3 | 7 daily, 4 weekly, 12 monthly |
| Repository integrity checks | Weekly | Each configured repository | Prunes according to that repository's policy |

| Recovery boundary  | Value                                                                                                                                                                        |
| ------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Backup storage | Encrypted Restic; primary buckets `backup-<project>` on SeaweedFS `hel1`; AWS application repositories in the administrator-created unversioned `llunde-pyparser-bucket-backups`, one prefix per project; control recovery in `llunde-pyparser-bucket/restic/platform/control` |
| Upload order | Preflight each repository and check AWS snapshot age; quiesce writers, export once, verify, resume writers, then upload to repositories that are due; prune only after successful upload; a failed destination does not prevent the other upload |
| Snapshot limits | Primary application repositories retain the latest 3 snapshots; AWS application repositories retain 1; no versioning or object lock retains deleted application backup data |
| Control history | Noncurrent control recovery object versions expire after 90 days |
| Credentials        | Separate project prefixes and separate maintenance authority; SOPS recovery available from Macie or Archie                                                                   |
| Independent copies | Verified migration archives on Macie and Archie                                                                                                                              |
| Volume policy      | Retained local volumes; important application data currently resides on `fredrir-09`                                                                                         |
| Writer quiescence  | Backup jobs scale writers to zero and back; writer Deployments omit `replicas` so Flux does not resume them mid-export; Writer alerts allow 35 minutes for parser dataset export and 15 minutes for Y |
| Node loss          | Local volumes do not migrate automatically; restore to replacement storage after fencing the old writer                                                                      |
| Verification       | Parser, Y, portfolio, Attic and native K3s datastore restored independently; row/file checks passed                                                                          |
| Cache recovery     | Builds can bootstrap independently; cache objects may be rebuilt                                                                                                             |
| Disposable data    | Main Llunde seeded database and observability history                                                                                                                        |
| Recovery time      | Provider provisioning plus restore time; no automatic cross-provider failover claim                                                                                          |

Use native `restic snapshots`, `restic restore` and database restore tools with the matching encrypted credentials; primary repositories are reachable only from the project's `repository-maintenance` pods, with `RESTIC_CACERT=/usr/local/share/object-store/ca.crt` from the tools image. Restore PostgreSQL with `pg_restore`, MongoDB with `mongorestore`, and K3s with its matching version and server token. Etcd snapshots contain no application volume data.

## Provider resources

| Name             | Value                                                                                                  |
| ---------------- | ------------------------------------------------------------------------------------------------------ |
| Inputs           | [Production settings](../tofu/production.tfvars.json)                                                  |
| Backend          | Existing S3 `llunde-pyparser-bucket`, key `tofu-state/infra.tfstate`, `eu-north-1`                     |
| Credentials      | AWS credential chain; command-scoped `TF_VAR_hcloud_token`, `CLOUDFLARE_API_TOKEN`, `TF_VAR_platform_mail_recipient` |
| Renames          | Declarative `moved` blocks preserve existing resource identities                                       |
| Resource changes | Review the saved plan before applying it                                                               |
| Media            | Existing AWS bucket and media identifiers remain unchanged                                             |

```sh
umask 077
mkdir -p .infra/plans
tofu -chdir=tofu init -lockfile=readonly -input=false
TF_VAR_hcloud_token="$(sops decrypt --extract '["HCLOUD_TOKEN"]' secrets/operator.sops.yaml)" \
CLOUDFLARE_API_TOKEN="$(sops decrypt --extract '["CLOUDFLARE_API_TOKEN"]' secrets/operator.sops.yaml)" \
TF_VAR_platform_mail_recipient="$(sops decrypt --extract '["PLATFORM_ALERT_RECIPIENT"]' secrets/operator.sops.yaml)" \
tofu -chdir=tofu plan -input=false \
  -var-file=production.tfvars.json \
  -out=../.infra/plans/production.tfplan
tofu -chdir=tofu show ../.infra/plans/production.tfplan
```
