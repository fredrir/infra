# Shared platform

| Name | Value |
| --- | --- |
| Repository | `fredrir/infra` |
| Inventory | [Ansible production inventory](../ansible/inventory/production.yml) |
| Hosts | Ubuntu 26.04 LTS; [node identities and roles](nodes.md) |
| Cluster | K3s; three embedded-etcd servers and two shared workers |
| Host configuration | Ansible, native K3s `config.yaml`, systemd and nftables |
| Cluster configuration | Flux, HelmRelease, Kustomize and ordinary YAML |
| Infrastructure credentials | Doppler `infra → ops` |
| Runtime secrets | SOPS + age; separate Macie, Archie and Flux recipients |
| Scheduling | Kubernetes requests, limits, quotas, priorities and protected runtime capability labels |
| Images | GHCR, immutable digests |
| Version pins | [Platform versions](../platform/versions.yaml), role defaults, image Dockerfiles, `flake.lock`; runner image digests in the runner sets and the [admission policy](../platform/components/policy/admission.yaml) |

## Ownership

| Owner | Resources |
| --- | --- |
| OpenTofu | Provider machines, private network, DNS, tunnels and restricted backup/mail IAM |
| Ansible | Host packages, SSH, firewall, Tailscale, K3s, containerd and native services |
| Flux platform reconciler | Shared services, project namespaces, network policy, admission and encrypted secrets |
| Project repository | Source, native tests, Dockerfiles and a pinned shared workflow caller |
| Deployment PR | Verified image digest and source revision, limited to the configured application |
| Application owner | Database schema compatibility and recovery requirements |

Provider APIs provision machines; an existing SSH-accessible machine enters through Ansible inventory. Workers use encrypted Tailscale connectivity across providers. Etcd stays on the three colocated control servers. Macie and Archie are administration and recovery machines.

## Services

| Address | Service |
| --- | --- |
| `fredrir.com` | Reserved for the future `fredrir/fredrir` application |
| `fredrir.com/logs` | Redirect to Grafana |
| `grafana.fredrir.com` | Authenticated Grafana, Prometheus metrics, Loki logs and Tempo traces |
| `cache.fredrir.com/infra` | Authenticated, signed Attic Nix cache |
| `admin.fredrir.com` | Reserved for the future dashboard |
| `fredrir.no` | Centrally managed DNS; purpose unassigned |
| `hansteen.dev` | Portfolio |
| `yeeter.no` | Y |
| `parser.llunde.no`, `external.llunde.no` | Parser review and shared media |
| `llunde.no`, `api.llunde.no` | Work-in-progress Llunde frontend/backend |

| Shared component | Configuration |
| --- | --- |
| Ingress | Two Traefik instances and two Cloudflare Tunnel instances; project ingress class `platform` |
| Metrics | Prometheus; 15 days or 18 GB |
| Logs | Alloy → Loki; 7 days |
| Traces | Alloy → Tempo; 7 days, bounded ingestion and sensitive attribute removal |
| Nix cache | Attic; 30-day retention, scoped client tokens and server-held signing key |
| Independent monitoring | Gatus on `fredrir-06`; 13 endpoint checks and five authenticated backup heartbeats |
| Email | `alerts@fredrir.com`; [credentials and operation](mail-alerts.md) |

## CI and deployments

| Name | Value |
| --- | --- |
| Runner registration | Separate `fredrir-infra-runners` GitHub App; encrypted credentials in ARC namespaces |
| Container builds | Ephemeral ARC runners using gVisor and native BuildKit |
| Nix builds | Ephemeral ARC runners using Kata; `sandbox=true`, no sandbox fallback |
| Runner permissions | No host sockets, host paths or Kubernetes API token |
| Build egress | Cluster DNS and TCP 443 only; Dockerfiles must use `https://` package sources |
| Runner scratch | 10Gi requested, 60Gi limit ephemeral storage; BuildKit runners prefer the non-Kata worker |
| Shared images | [Infrastructure images workflow](../.github/workflows/images.yml): runner BuildKit, runner Nix, backup tools and `platform-caddy`; BuildKit, runc, restic and Caddy compile from pinned upstream commits with patched Go modules because published binaries carry fixable findings |
| Untrusted public PRs | No PR workflow triggers; GitHub approval required for every external contributor; do not approve external runs |
| Approved source | Protected `main` pushes with matching numeric repository and owner identities |
| Workflow reuse | Immutable `fredrir/infra/.github/workflows/build-image.yml@<commit>` |
| Release authorization | OctoSTS installed only on `fredrir/infra`; exact repository, event and immutable workflow claims |
| Deployment mappings | [Repository image mappings](../.github/deployments) |
| Promotion | Verify GitHub attestations for public images or keyless Cosign signatures for private images, then merge the reviewed digest PR for Flux to reconcile from `main` |
| Rollback | Revert the deployment commit; check database schema compatibility first |
| Native ARM | Unavailable until an ARM worker and its runtime/images pass qualification |

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

| Generated files | Destination |
| --- | --- |
| `project/` | `platform/projects/example/`; include it in the projects Kustomization |
| `runner/` | `platform/components/runners/example/`; include it in the runners Kustomization |
| `infrastructure/.github/` | Infrastructure deployment mapping and OctoSTS trust policy |
| `caller/.github/workflows/build.yaml` | Project repository |
| Application credentials | Add encrypted `project-registry` and `project-runtime` Secrets |
| Runner credentials | Add the encrypted ARC registration Secret in the new runner namespace |
| Activation | Review generated YAML, resource limits and secrets, then raise workload replicas from zero |

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

| Prerequisite | Value |
| --- | --- |
| SSH | Verified host key, administrator public keys, root or passwordless sudo |
| Transport | Enrolled Tailscale role; native OpenSSH, embedded Tailscale SSH disabled |
| New-node enrollment | [Enrollment helper](../scripts/operations/tailscale_enrollment.py) and [bootstrap playbook](../ansible/tailscale-bootstrap.yml) |
| Enrollment credentials | Doppler `infra/ops/TAILSCALE_ENROLL_CLIENT_ID` and `TAILSCALE_ENROLL_CLIENT_SECRET`; owner tag `tag:platform-enrollment` |
| K3s credentials | Separate server/agent tokens in private `/etc/rancher/k3s/` files |
| Registration seed | Healthy, already clustered control server; default fresh-cluster initializer `fredrir-07` |
| Control network | `10.60.0.5`, `10.60.0.7`, `10.60.0.8`; etcd stays private |
| Admin API | Verified kubeconfig; API port 6443 through approved narrow subnet routes |
| Maintenance | Serial upgrades; reboot only when required; verify new boot identity, API and NodeReady before proceeding |

```sh
export KUBECONFIG=/path/to/private/kubeconfig
kubectl get nodes -o wide
flux get all --all-namespaces
kubectl get pods --all-namespaces
kubectl get cronjobs --all-namespaces
```

## Backups and recovery

| Data | Cadence | Contents |
| --- | --- | --- |
| Parser | Every 6 hours | PostgreSQL logical dump and local files; writers quiesced together |
| Y | Every 6 hours | MongoDB dump and local media; writers quiesced together |
| Portfolio | Hourly | PostgreSQL logical dump; existing AWS media retained |
| Control plane | Every 6 hours | Native etcd snapshot, exact server/agent tokens, K3s configuration and encrypted repository copy of Kubernetes Secrets |
| Attic | Hourly | Consistent SQLite export including signing identity |
| Restic maintenance | Weekly | Full repository read/integrity checks and retention |
| AWS application media | Preserved in place | Independent user-managed Google Drive copy |

| Recovery boundary | Value |
| --- | --- |
| Backup storage | Encrypted Restic repositories outside the cluster; interchangeable backend tooling |
| Credentials | Separate project prefixes and separate maintenance authority; SOPS recovery available from Macie or Archie |
| Independent copies | Verified migration archives on Macie and Archie |
| Volume policy | Retained local volumes; important application data currently resides on `fredrir-09` |
| Writer quiescence | Backup jobs scale writers to zero and back; writer Deployments omit `replicas` so Flux does not resume them mid-export; `BackupWriterLeftScaledDown` alerts after 15 minutes |
| Node loss | Local volumes do not migrate automatically; restore to replacement storage after fencing the old writer |
| Verification | Parser, Y, portfolio, Attic and native K3s datastore restored independently; row/file checks passed |
| Cache recovery | Builds can bootstrap independently; cache objects may be rebuilt |
| Disposable data | Main Llunde seeded database and observability history |
| Recovery time | Provider provisioning plus restore time; no automatic cross-provider failover claim |

Use native `restic snapshots`, `restic restore` and database restore tools with the matching encrypted credentials. Restore PostgreSQL with `pg_restore`, MongoDB with `mongorestore`, and K3s with its matching version and server token. Etcd snapshots contain no application volume data.

## Provider resources

| Name | Value |
| --- | --- |
| Inputs | [Production settings](../tofu/production.tfvars.json) |
| Backend | Existing S3 `llunde-pyparser-bucket`, key `tofu-state/infra.tfstate`, `eu-north-1` |
| Credentials | AWS credential chain, `HCLOUD_TOKEN`, `CLOUDFLARE_API_TOKEN`; private `TF_VAR_platform_mail_recipient` |
| Renames | Declarative `moved` blocks preserve existing resource identities |
| Resource changes | Review the saved plan before applying it |
| Media | Existing AWS bucket and media identifiers remain unchanged |

```sh
umask 077
mkdir -p .infra/plans
tofu -chdir=tofu init -lockfile=readonly -input=false
tofu -chdir=tofu plan -input=false \
  -var-file=production.tfvars.json \
  -out=../.infra/plans/production.tfplan
tofu -chdir=tofu show ../.infra/plans/production.tfplan
```
