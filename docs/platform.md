# Shared platform

| Name                       | Value                                                                                                                                                                                                            |
| -------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Repository                 | `fredrir/infra`                                                                                                                                                                                                  |
| Inventory                  | [Ansible production inventory](../ansible/inventory/production.yml)                                                                                                                                              |
| Hosts                      | Ubuntu 26.04 LTS; [node identities and roles](nodes.md)                                                                                                                                                          |
| Cluster                    | K3s; three embedded-etcd servers and two shared workers                                                                                                                                                          |
| Host configuration         | Ansible, native K3s `config.yaml`, systemd and nftables                                                                                                                                                          |
| Cluster configuration      | Flux, HelmRelease, Kustomize and ordinary YAML                                                                                                                                                                   |
| Infrastructure credentials | Doppler `infra → ops`                                                                                                                                                                                            |
| Runtime secrets            | SOPS + age; separate Macie, Archie and Flux recipients                                                                                                                                                           |
| Scheduling                 | Kubernetes requests, limits, quotas, priorities, protected runtime capability labels and per-worker CI slots                                                                                                     |
| Images                     | GHCR, immutable digests                                                                                                                                                                                          |
| Version pins               | [Platform versions](../platform/versions.yaml), role defaults, image Dockerfiles, `flake.lock`; runner image digests in the runner sets and the [admission policy](../platform/components/policy/admission.yaml) |

## Ownership

| Owner                    | Resources                                                                            |
| ------------------------ | ------------------------------------------------------------------------------------ |
| OpenTofu                 | Provider machines, private network, DNS, tunnels and restricted backup/mail IAM      |
| Ansible                  | Host packages, SSH, firewall, Tailscale, K3s, containerd and native services         |
| Flux platform reconciler | Shared services, project namespaces, network policy, admission and encrypted secrets |
| Project repository       | Source, native tests, Dockerfiles and a pinned shared workflow caller                |
| Deployment PR            | Verified image digest and source revision, limited to the configured application     |
| Application owner        | Database schema compatibility and recovery requirements                              |

Provider APIs provision machines; an existing SSH-accessible machine enters through Ansible inventory. Workers use encrypted Tailscale connectivity across providers. Etcd stays on the three colocated control servers. Macie and Archie are administration and recovery machines.

## Services

| Address                                  | Service                                                               |
| ---------------------------------------- | --------------------------------------------------------------------- |
| `fredrir.com`                            | Reserved for the future `fredrir/fredrir` application                 |
| `grafana.fredrir.com`                    | Authenticated Grafana, Prometheus metrics, Loki logs and Tempo traces |
| `cache.fredrir.com`                      | Authenticated, signed Attic Nix cache                                 |
| `pkgs.fredrir.com`                       | Signed apt, rpm and apk repositories and `install.sh`                 |
| `admin.fredrir.com`                      | Reserved for the future dashboard                                     |
| `fredrir.no`                             | Centrally managed DNS; purpose unassigned                             |
| `hansteen.dev`                           | Portfolio                                                             |
| `yeeter.no`                              | Y                                                                     |
| `parser.llunde.no`, `external.llunde.no` | Parser review and shared media                                        |
| `llunde.no`, `api.llunde.no`             | Work-in-progress Llunde frontend/backend                              |

| Shared component       | Configuration                                                                                                                                                         |
| ---------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Ingress                | Two Traefik instances and two Cloudflare Tunnel instances; project ingress class `platform`                                                                           |
| Metrics                | Prometheus; 15 days or 18 GB                                                                                                                                          |
| Logs                   | Alloy → Loki; 7 days                                                                                                                                                  |
| Traces                 | Alloy → Tempo; 7 days, bounded ingestion and sensitive attribute removal                                                                                              |
| Nix cache              | Attic; 30-day retention, scoped client tokens and server-held signing key                                                                                             |
| Build cache            | Garage S3 on `fredrir-09`; `ci-<project>-main` 20 GiB (sccache, `target/` archive, BuildKit layers), `ci-<project>-release` 10 GiB, 14-day expiry; `toolchains` 5 GiB |
| Independent monitoring | Gatus on `fredrir-06`; 13 endpoint checks and five authenticated backup heartbeats                                                                                    |
| Email                  | `alerts@fredrir.com`; [credentials and operation](mail-alerts.md)                                                                                                     |

## CI and deployments

| Name                  | Value                                                                                                                                                                                                                                                                                           |
| --------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Runner registration   | Separate `fredrir-infra-runners` GitHub App; encrypted credentials in ARC namespaces                                                                                                                                                                                                            |
| Container builds      | Ephemeral ARC runners using gVisor and native BuildKit; BuildKit cannot run under Kata here because virtiofs rejects the xattr reads its content hashing needs; layer cache `type=s3,mode=max` in `ci-<project>-main`, weekly manifest rotation, caller input `layer-cache: false` disables it  |
| Nix builds            | Ephemeral ARC runners using Kata; `sandbox=true`, no sandbox fallback                                                                                                                                                                                                                           |
| Rust builds           | Ephemeral gVisor pools on the [Rust runner image](../images/runner-rust/Containerfile): `rust-pr-amd64`, `rust-amd64`, `rust-release-amd64`; sccache on the build cache; `target/` archive per toolchain, written by `rust-amd64` only                                                          |
| CI capacity           | `infra.fredrir.com/ci-slot`: `fredrir-04` 1, `fredrir-09` 3; source label `infra.fredrir.com/ci-slots`, restored by `arc-system/ci-slots`                                                                                                                                                       |
| Runner permissions    | No host sockets, host paths or Kubernetes API token                                                                                                                                                                                                                                             |
| Build egress          | Cluster DNS and TCP 443; Rust and layer-cache pools add the build cache on TCP 3900; `ci-infra` BuildKit adds the Flux receiver on TCP 9292; `publish-amd64` adds TCP 22 to AUR; Dockerfiles must use `https://` package sources                                                                |
| Job timeout           | 45 minutes; callers with long native test suites pass `timeout-minutes`                                                                                                                                                                                                                         |
| Runner scratch        | 10Gi requested, 60Gi limit ephemeral storage; Rust pools 20Gi/80Gi; BuildKit runners 1/4 CPU and prefer the non-Kata worker; the parser pool gets 40Gi/200Gi on `fredrir-09`                                                                                                                    |
| Shared images         | [Infrastructure images workflow](../.github/workflows/images.yml): runner BuildKit, runner Nix, runner Rust, backup tools and `platform-caddy`; BuildKit, runc, restic and Caddy compile from pinned upstream commits with patched Go modules because published binaries carry fixable findings |
| Fork PRs              | Never run: job-level guard, pool job-started hook and approval required for every external contributor                                                                                                                                                                                          |
| Approved source       | Protected `main` pushes; same-repository PRs on `rust-pr-amd64`; protected `v*` tags and `main` dry runs on `rust-release-amd64`; matching numeric repository and owner identities                                                                                                              |
| Workflow reuse        | Immutable `fredrir/infra/.github/workflows/{build-image,rust-ci,rust-auto-tag,rust-release,packages-publish}.yml@<commit>`                                                                                                                                                                      |
| Pin changes           | A new shared workflow or shared recipe commit re-pins all callers and the OctoSTS policies to that commit together                                                                                                                                                                              |
| Release authorization | OctoSTS on `infra`, onboarded projects, `packages`, `homebrew-tap`, `homebrew-nsql` and `nur-packages`; policies pin workflow path, repository id, ref and environment; project deploy identities hold `actions: write` on `infra`, only `deploy.yml` on `main` holds `contents: write`         |
| Deployment mappings   | [Repository image mappings](../.github/deployments)                                                                                                                                                                                                                                             |
| Promotion             | `build-image.yml` dispatches [`deploy.yml`](../.github/workflows/deploy.yml); it verifies the GitHub attestation (public) or keyless Cosign signature (private) against the pinned workflow commit, pushes the digest to `main` and notifies the Flux `deploy` receiver                         |
| Rollback              | Revert the deployment commit; check database schema compatibility first                                                                                                                                                                                                                         |
| Native ARM            | Rust targets cross-compile with cargo-zigbuild; container images and native tests stay amd64 until an ARM worker passes qualification                                                                                                                                                           |

A project caller contains only its build inputs and a pinned reusable workflow. The infrastructure repository owns authentication, publication and deployment PR logic.

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
    uses: fredrir/infra/.github/workflows/build-image.yml@<reviewed-40-character-commit>
    with:
      image: ghcr.io/fredrir/example
      test-command: <native-project-test-command>
```

## Rust projects

| Name           | Value                                                                                                                                                          |
| -------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Registry       | [Rust projects](../.github/rust-projects.yaml)                                                                                                                 |
| CI             | [`rust-ci.yml`](../.github/workflows/rust-ci.yml): fmt, clippy, nextest, doc tests, minimal build, MSRV, cargo audit                                           |
| Versioning     | [`rust-auto-tag.yml`](../.github/workflows/rust-auto-tag.yml): patch bump, git-cliff, `v*` tag                                                                 |
| Release        | [`rust-release.yml`](../.github/workflows/rust-release.yml): GoReleaser OSS and cargo-zigbuild                                                                 |
| Targets        | `x86_64`/`aarch64` Linux gnu (glibc 2.28) and musl (static); `x86_64`/`aarch64` macOS (SDK in `toolchains`)                                                    |
| Channels       | GitHub Release, crates.io (trusted publishing), `fredrir/homebrew-tap`, AUR `<name>` and `<name>-bin`, `fredrir/nur-packages`, `pkgs.fredrir.com`              |
| Publisher      | `fredrir/packages` → [`packages-publish.yml`](../.github/workflows/packages-publish.yml); dispatch after each release, daily reconcile, weekly full smoke test |
| Package trust  | Attested by the release workflow on an infra `main` revision; latest 3 stable releases per project                                                             |
| Public keys    | [GPG](../scripts/packages/keys/fredrir.asc), [apk](../scripts/packages/keys/fredrir.rsa.pub)                                                                   |
| Private repos  | CI only                                                                                                                                                        |
| Release config | `[package.metadata.release]`: `maintainer`, `section`, `features`, `extra-files`, `optional`, `depends`                                                        |

```sh
nix develop
uv run --frozen infra onboard-rust fredrir/example \
  --project example \
  --workflow-ref "$workflow_revision" \
  --output .infra/onboarding/example
```

| Generated                                   | Destination                                     |
| ------------------------------------------- | ----------------------------------------------- |
| `platform/components/runners/example/`      | Written in place; three pools and cache secrets |
| `platform/components/build-cache/projects/` | Written in place; provisioner keys              |
| `.github/rust-projects.yaml`                | Written in place                                |
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
bash scripts/operations/macos_sdk.sh package .infra/macos-sdk
SOPS_AGE_KEY_FILE=/path/to/age-key.txt bash scripts/operations/macos_sdk.sh upload .infra/macos-sdk/MacOSX<version>.sdk.tar.zst
```

## Onboarding

```sh
nix develop
uv run --frozen infra onboard fredrir/example \
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
| `project/`                            | `platform/projects/example/`; include it in the projects Kustomization                     |
| `runner/`                             | `platform/components/runners/example/`; include it in the runners Kustomization            |
| `infrastructure/.github/`             | Infrastructure deployment mapping (`visibility`, image paths) and OctoSTS trust policy     |
| `caller/.github/workflows/build.yaml` | Project repository                                                                         |
| Application credentials               | Add encrypted `project-registry` and `project-runtime` Secrets                             |
| Runner credentials                    | Add the encrypted ARC registration Secret in the new runner namespace                      |
| Activation                            | Review generated YAML, resource limits and secrets, then raise workload replicas from zero |

```sh
uv run --frozen infra onboard-cache --project example
```

| Generated                                                             | Value                                |
| --------------------------------------------------------------------- | ------------------------------------ |
| `platform/components/runners/example/buildkit-cache.secret.sops.yaml` | Read-write key for `ci-example-main` |
| `platform/components/build-cache/projects/example.secret.sops.yaml`   | Provisioner keys                     |
| `components: [../buildkit-cache]`                                     | Cache environment and egress         |

| `infra` setting  | Value                                                                                   |
| ---------------- | --------------------------------------------------------------------------------------- |
| `main` ruleset   | [`main-ruleset.json`](../.github/main-ruleset.json); bypass: repository admin, Octo STS |
| Private projects | `.github/chainguard/infra-deploy.sts.yaml`: `packages: read` for `deploy.yml` on `main` |

The shared [project chart](../charts/project) supports web services, workers and scheduled jobs. Databases and durable files require explicit storage, backup and recovery configuration.

## Host operation

```sh
export ANSIBLE_CONFIG=ansible/ansible.cfg
export SOPS_AGE_KEY_FILE=/path/to/age-key.txt
uv run --frozen --group ci ansible-playbook ansible/site.yml --limit fredrir-NN
uv run --frozen --group ci ansible-playbook ansible/k3s.yml --limit fredrir-NN -e k3s_registration_server=fredrir-08
uv run --frozen --group ci ansible-playbook ansible/ci-runtimes.yml --limit fredrir-NN
uv run --frozen --group ci ansible-playbook ansible/maintenance.yml
uv run --frozen --group ci ansible-playbook ansible/external.yml --limit fredrir-06
```

| Prerequisite           | Value                                                                                                                           |
| ---------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| SSH                    | Verified host key, administrator public keys, root or passwordless sudo                                                         |
| Transport              | Enrolled Tailscale role; native OpenSSH, embedded Tailscale SSH disabled                                                        |
| New-node enrollment    | [Enrollment helper](../scripts/operations/tailscale_enrollment.py) and [bootstrap playbook](../ansible/tailscale-bootstrap.yml) |
| Enrollment credentials | Doppler `infra/ops/TAILSCALE_ENROLL_CLIENT_ID` and `TAILSCALE_ENROLL_CLIENT_SECRET`; owner tag `tag:platform-enrollment`        |
| K3s credentials        | Separate server/agent tokens in private `/etc/rancher/k3s/` files                                                               |
| Registration seed      | Healthy, already clustered control server; default fresh-cluster initializer `fredrir-07`                                       |
| Control network        | `10.60.0.5`, `10.60.0.7`, `10.60.0.8`; etcd stays private                                                                       |
| Admin API              | Verified kubeconfig; API port 6443 through approved narrow subnet routes                                                        |
| Maintenance            | Serial upgrades; reboot only when required; verify new boot identity, API and NodeReady before proceeding                       |

```sh
export KUBECONFIG=/path/to/private/kubeconfig
kubectl get nodes -o wide
flux get all --all-namespaces
kubectl get pods --all-namespaces
kubectl get cronjobs --all-namespaces
```

## Backups and recovery

| Data                  | Cadence            | Contents                                                                                                               |
| --------------------- | ------------------ | ---------------------------------------------------------------------------------------------------------------------- |
| Parser                | Every 6 hours      | PostgreSQL logical dump and local files; writers quiesced together                                                     |
| Y                     | Every 6 hours      | MongoDB dump and local media; writers quiesced together                                                                |
| Portfolio             | Hourly             | PostgreSQL logical dump; existing AWS media retained                                                                   |
| Control plane         | Every 6 hours      | Native etcd snapshot, exact server/agent tokens, K3s configuration and encrypted repository copy of Kubernetes Secrets |
| Attic                 | Hourly             | Consistent SQLite export including signing identity                                                                    |
| Restic maintenance    | Weekly             | Full repository read/integrity checks and retention                                                                    |
| AWS application media | Preserved in place | Independent user-managed Google Drive copy                                                                             |

| Recovery boundary  | Value                                                                                                                                                                        |
| ------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Backup storage     | Encrypted Restic repositories outside the cluster; interchangeable backend tooling                                                                                           |
| Credentials        | Separate project prefixes and separate maintenance authority; SOPS recovery available from Macie or Archie                                                                   |
| Independent copies | Verified migration archives on Macie and Archie                                                                                                                              |
| Volume policy      | Retained local volumes; important application data currently resides on `fredrir-09`                                                                                         |
| Writer quiescence  | Backup jobs scale writers to zero and back; writer Deployments omit `replicas` so Flux does not resume them mid-export; `BackupWriterLeftScaledDown` alerts after 15 minutes |
| Node loss          | Local volumes do not migrate automatically; restore to replacement storage after fencing the old writer                                                                      |
| Verification       | Parser, Y, portfolio, Attic and native K3s datastore restored independently; row/file checks passed                                                                          |
| Cache recovery     | Builds can bootstrap independently; cache objects may be rebuilt                                                                                                             |
| Disposable data    | Main Llunde seeded database and observability history                                                                                                                        |
| Recovery time      | Provider provisioning plus restore time; no automatic cross-provider failover claim                                                                                          |

Use native `restic snapshots`, `restic restore` and database restore tools with the matching encrypted credentials. Restore PostgreSQL with `pg_restore`, MongoDB with `mongorestore`, and K3s with its matching version and server token. Etcd snapshots contain no application volume data.

## Provider resources

| Name             | Value                                                                                                  |
| ---------------- | ------------------------------------------------------------------------------------------------------ |
| Inputs           | [Production settings](../tofu/production.tfvars.json)                                                  |
| Backend          | Existing S3 `llunde-pyparser-bucket`, key `tofu-state/infra.tfstate`, `eu-north-1`                     |
| Credentials      | AWS credential chain, `HCLOUD_TOKEN`, `CLOUDFLARE_API_TOKEN`; private `TF_VAR_platform_mail_recipient` |
| Renames          | Declarative `moved` blocks preserve existing resource identities                                       |
| Resource changes | Review the saved plan before applying it                                                               |
| Media            | Existing AWS bucket and media identifiers remain unchanged                                             |

```sh
umask 077
mkdir -p .infra/plans
tofu -chdir=tofu init -lockfile=readonly -input=false
tofu -chdir=tofu plan -input=false \
  -var-file=production.tfvars.json \
  -out=../.infra/plans/production.tfplan
tofu -chdir=tofu show ../.infra/plans/production.tfplan
```
