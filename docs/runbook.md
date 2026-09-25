# Runbook

| Operation                   | Entry point                                              |
| --------------------------- | -------------------------------------------------------- |
| Provision or adopt a server | [Provider resources](platform.md#provider-resources)     |
| Configure or update a host  | [Host operation](platform.md#host-operation)             |
| Add a project               | [Onboarding](platform.md#onboarding)                     |
| Deploy or roll back         | [CI and deployments](platform.md#ci-and-deployments)     |
| Recover data                | [Backups and recovery](platform.md#backups-and-recovery) |
| Inspect independent alerts  | [Gatus and email](mail-alerts.md)                        |

```sh
export KUBECONFIG=/path/to/private/kubeconfig
kubectl get nodes
flux get kustomizations --all-namespaces
flux get helmreleases --all-namespaces
kubectl get pods --all-namespaces
kubectl get cronjobs --all-namespaces
```

| Recovery step                       | Required result                                                |
| ----------------------------------- | -------------------------------------------------------------- |
| Stop or fence the old writer        | No competing database or media writer                          |
| Select a verified Restic snapshot   | Matching application, credentials and recovery timestamp       |
| Restore into separate storage       | Existing recovery source retained                              |
| Check native data                   | Database counts/integrity, media hashes and application access |
| Change deployment storage and image | Reviewed Git change; Flux health checks pass                   |
| Resume writes                       | One active writer; fresh backup succeeds                       |

Etcd recovery requires the snapshot's matching K3s version and server token. Application volumes need their own restore.

## Reconciliation

| Stage | Entry point | Result |
| --- | --- | --- |
| Pull request | `infra ci prepare-validation`, `infra ci validate`, `infra reconcile plan --base BASE_SHA` | Affected declarations, OpenTofu expansion and final-state plans, Flux rendering, Ansible task lists |
| Merge to `main` | `infra reconcile apply` | Fresh plan for the exact checkout; apply against the last successful revision |
| Scheduled verification | `infra reconcile verify --deep --report REPORT` | Read-only verification and check-mode comparison of OpenTofu and each host playbook at minute 47 every hour (UTC); a report listing differences dispatches one full reconciliation of `main` |
| On-demand verification | `gh workflow run reconcile.yml --ref main -f verify=true` | The scheduled verification on demand; differences are reported without dispatching a reconciliation |
| Verification | `infra reconcile verify` | Reconciliation lock and recorded status, exact Flux revision, observed generations, Helm readiness, host checks, Grafana configuration and HTTP health, frontend revision; every comparison runs when another fails |
| Status | `infra reconcile status` | Desired revision, successfully applied revision, failing stage and stage durations |

| Verification report | Value |
| --- | --- |
| Differences | OpenTofu plan changes, host tasks changed in check mode, runner drift, Flux objects that differ from or have not applied the published revision, unpublished deploying changes, an incomplete or failed recorded reconciliation |
| Errors | Unreachable hosts, failed host tasks, playbooks that could not be compared, API failures, readiness, timeouts, suspended Flux objects, replica counts a manifest does not declare |
| Held or unreadable reconciliation lock | Error; comparisons skipped, or discarded when the lock is taken during them |
| No state bucket access | Error; comparisons skipped |
| Unpublished deploying changes | Difference; comparisons skipped |
| Repair not dispatched | Push reconciliation on `main` with an incomplete `reconcile / apply` job; latest bot dispatch for the commit ended in `failure`, `timed_out` or `startup_failure`, or started within six hours and was not cancelled |
| Repair cap reset | New commit on `main` |

| Owner | Managed state |
| --- | --- |
| OpenTofu | Cloudflare, provider resources and IAM identities in `tofu/` |
| Flux | Kubernetes objects from the `production` branch |
| Ansible | Enrolled transport, host configuration, build VMs, runners, backups and independent monitoring |
| Go coordinator | Deployment ordering, the `production` branch and reconciliation records |

| Setting | Value |
| --- | --- |
| Grafana hostname declaration | `platform/clusters/production/settings.yaml`: `data.GRAFANA_HOST` |
| Rename sequence | Retain state-owned DNS and tunnel routes; add the new route; update Flux and Gatus; verify the active Grafana URL; retire previous routes |
| Merge selection | Diff against the last successfully applied revision; incomplete attempts force all systems |
| Hostname-only deployment | Skip unrelated host configuration; run the Gatus role |
| State | `s3://llunde-pyparser-bucket/reconciliation/production/status.json` |
| Cross-client lock | Conditional S3 writes to `reconciliation/production/lock.json` |
| Run deadline / apply job timeout / abandoned lock expiry | 90 minutes / 120 minutes / 2 hours |
| OpenTofu locking | S3 lockfile retained; acquisition timeout 5 minutes |
| Failed verification | Old routes retained; applied revision unchanged |
| Failed retirement | Applied revision unchanged; the next attempt reads actual OpenTofu state |
| Reports | GitHub job summary and `reconciliation-RUN_ID-ATTEMPT` artifact |
| Sensitive plans | Private temporary directory; never uploaded as workflow artifacts |
| Fork pull requests | Declaration validation only; live-plan check fails until changes are on a trusted repository branch |

```sh
go build -o .infra/bin/infra ./cmd/infra
.infra/bin/infra reconcile plan --base BASE_SHA
.infra/bin/infra reconcile apply --report .infra/reconciliation/status.json
.infra/bin/infra reconcile status
.infra/bin/infra reconcile verify
.infra/bin/infra reconcile apply --full
```

### Initial activation

| Order | Required action |
| --- | --- |
| 1 | Prepare an administrator session with OpenTofu backend/provider access, Kubernetes access, existing host SSH identities and SOPS decryption |
| 2 | Merge the reconciliation implementation without a simultaneous hostname change |
| 3 | Apply the IAM policies from `tofu/reconciliation.tf` using an administrator OpenTofu session; let Flux install the Kubernetes identities |
| 4 | Confirm the IAM users, managed policy attachments and Kubernetes service accounts exist; point Flux at `production` |
| 5 | Create GitHub environments `infrastructure-plan` and `infrastructure-apply`; restrict `infrastructure-apply` to `main` without deployment reviewers |
| 6 | Populate the scoped Doppler configurations below; install their read-only service tokens as `DOPPLER_TOKEN` in the matching GitHub environments |
| 7 | Run `ansible-playbook reconciliation-identity.yml` with administrator SSH access; retain existing administrator keys |
| 8 | Apply `tailscale/policy.hujson`; create the environment-bound OIDC identities below |
| 9 | Run the workflow manually and confirm `desired_revision == applied_revision` with `stage == complete` |

These activation steps provision external credentials once; merge, verification and dispatched reconciliation runs fetch credentials from Doppler.

| GitHub environment | Doppler project / config | GitHub secret |
| --- | --- | --- |
| `infrastructure-plan` | `infra / prd_reconciliation_plan` | `DOPPLER_TOKEN`, read-only access to this config |
| `infrastructure-apply` | `infra / prd_reconciliation_apply` | `DOPPLER_TOKEN`, read-only access to this config |

| Doppler secret | `prd_reconciliation_plan` | `prd_reconciliation_apply` |
| --- | --- | --- |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | Keys for `/automation/infra-reconciliation-plan` | Keys for `/automation/infra-reconciliation-apply` |
| `CLOUDFLARE_API_TOKEN` | Read managed DNS zones and account tunnels | Edit managed DNS zones and account tunnels |
| `HCLOUD_TOKEN` | Read managed Hetzner project | Read/write managed Hetzner project |
| `KUBE_CONFIG` | `flux-system/infrastructure-plan` identity | `flux-system/infrastructure-apply` identity |
| `PLATFORM_MAIL_RECIPIENT` | Private OpenTofu mail recipient | Same recipient |
| `SSH_PRIVATE_KEY` | Unset | Dedicated key for managed hosts and build guest |
| `SSH_KNOWN_HOSTS` | Unset | Verified Tailnet host keys, `fredrir-06` and `infra-build-09` aliases |
| `SOPS_AGE_KEY` | Unset | Decrypt host monitoring and backup credentials |
| `RUNNER_APP_ID`, `RUNNER_APP_PRIVATE_KEY` | Unset | Existing runner GitHub App; mint a short-lived installation token with repository administration permission |

| OIDC setting | `infrastructure-plan` | `infrastructure-apply` |
| --- | --- | --- |
| GitHub environment variable | `TAILSCALE_CLIENT_ID` | `TAILSCALE_CLIENT_ID` |
| Issuer | `https://token.actions.githubusercontent.com` | Same issuer |
| Subject | `repo:fredrir@114402558/infra@1328085692:environment:infrastructure-plan` | `repo:fredrir@114402558/infra@1328085692:environment:infrastructure-apply` |
| Audience | `infra-reconciliation-plan` | `infra-reconciliation-apply` |
| Immutable identity | Owner `114402558`, repository `1328085692` in subject | Same identity |
| Workflow claim | Unset | `workflow_ref=fredrir/infra/.github/workflows/reconcile.yml@refs/heads/main` |
| Scope / tag | `auth_keys` / `tag:infra-plan` | `auth_keys` / `tag:infra-apply` |
| Device lifetime | Ephemeral; removed after job completion | Same lifetime |

| Credential boundary | Value |
| --- | --- |
| Secret source | Doppler; operational credentials are fetched per job and are not duplicated in GitHub environment secrets |
| Doppler scope | Separate config-scoped read tokens; neither token can edit secrets or read `infra/ops` |
| Doppler parent config | `infra/prd` contains no credentials; reconciliation configs contain only their scoped identities |
| AWS identity policies | `tofu/reconciliation.tf`; attached managed policies; deployment identities cannot change their own grants |
| Kubernetes identities | `platform/components/policy/reconciliation.yaml` |
| Kubernetes tokens | Controller-populated `infrastructure-plan-credentials` and `infrastructure-apply-credentials` Secrets in `flux-system` |
| Kubernetes API address | Reachable control-plane Tailnet address with a matching certificate; include its CA in each kubeconfig |
| Kubernetes apply scope | Read Flux resources; patch reconciliation annotations; admission rejects spec changes |
| Tailnet plan scope | Control-plane API only |
| Tailnet apply scope | Control-plane API and SSH to managed hosts |
| Host SSH key | `ansible/files/reconciliation.pub`; maintained by `ansible/reconciliation-identity.yml` |
| CI SOPS recipient | Added only to host monitoring and backup secret files |
| Provider-policy changes or revoked credentials | Administrator repair required |

```sh
doppler configs tokens create github-reconciliation-plan --project infra --config prd_reconciliation_plan --access read --plain | gh secret set DOPPLER_TOKEN --env infrastructure-plan
doppler configs tokens create github-reconciliation-apply --project infra --config prd_reconciliation_apply --access read --plain | gh secret set DOPPLER_TOKEN --env infrastructure-apply
gh workflow run reconcile.yml --ref main
```

### Recovery

| Failure | Recovery |
| --- | --- |
| Missing credentials or private connectivity | Repair the environment identity, host enrollment or Tailnet policy; rerun the workflow |
| Newer merge supersedes a queued run | Reconcile current `main`; stale runs cannot publish or report success |
| On-demand or hourly verification supersedes a run queued in the `infrastructure-production` concurrency group | When the superseded run carried deploying changes, the superseding hourly verification, or the next hourly one after an on-demand verification, reports the unapplied revision and dispatches a full reconciliation; dispatch `verify=true` when no apply is queued |
| Failed apply or verification | Rerun the workflow or run `infra reconcile apply --full` from a clean current `main` checkout |
| Process terminated without lock cleanup | Hourly verification reports the held lock until its recorded expiry, then the incomplete reconciliation as a difference |
| Remaining OpenTofu drift | Inspect the final plan; nonzero drift keeps the run failed |
| Image publication succeeds | Check the separate reconciliation workflow for production readiness |

Do not remove an active reconciliation or OpenTofu lock while its writer is running.

## CI execution and runner admission

Pushes and pull requests enter through `reconcile.yml`, which shares one verified CLI build with reusable checks and image planning.
Reconciliation starts independently and installs the pinned release when the checkout's CLI input digest matches; changed CLI inputs wait for the shared build and verify its input digest and binary checksum.
Apply retains declaration validation and live preflight, and the parent retains the existing Tailscale `workflow_ref` identity.
`infra reconcile requirements` compares the durable applied revision with the checkout before setup; application-only changes skip OpenTofu, Ansible, SSH and runner-registration setup, while tooling-only changes and incomplete or failed reconciliation require full tooling.
Native signed S3 state requests retain conditional lease creation, takeover and release, server-side encryption and durable status updates; AWS CLI credential resolution remains available when environment credentials are absent.
`performance.yml` records completed attempt timings, including failures, without checking out or executing the observed revision.

`build_runner_job_slots` limits simultaneous complete jobs across the build VM's repository listeners when `build_runner_admission_enabled` is true.
The start hook waits for a lease owned by its `Runner.Worker` PID and process start time; the completion hook releases it, and a later attempt reclaims leases from exited workers.
Admission wait is reported separately in hook output and consumes the job timeout.

Install the pinned CLI release before enabling admission, and drain active jobs before changing runner service overrides.
For rollback, disable admission and reconcile idle listeners before downgrading the CLI.
Do not delete leases while their workers are running; corrupted lease state fails admission until an operator repairs it with listeners drained.

| Runner upgrade   | Value                                                                                                     |
| ---------------- | --------------------------------------------------------------------------------------------------------- |
| Pin update       | Renovate pull request with the release's published sha256; digest-only updates disabled                   |
| Drain            | Replaced runner's custom labels removed; waits until GitHub reports it idle and its `Runner.Worker` exits |
| Drain deadline   | `build_runner_drain_minutes` (50), shared by all runners in one play                                      |
| Busy at deadline | Play fails; runner keeps its job and binaries                                                             |
| Labels restored  | After its replacement, including failed replacements                                                      |
| Job routing      | `dagger-amd64` or `infra-trusted`; `self-hosted`, `Linux` and `X64` stay during a drain                   |

A changed digest for an unchanged version is not proposed; investigate it before editing the pin.

The Rust reusable workflow accepts `build-output-cache: false` to skip target archive restore and publication while retaining compiler `sccache`.
`nsql` uses this setting because the measured archive refresh cost exceeded the compilation saved; identical-output reruns benefited from archive reuse.
Compare `rust-cache-restore`, `rust-preparation`, `rust-cache-save` and total job duration before changing this setting.
The aggregate check budget remains ten seconds, and cache publication still requires a successful trusted build.

Use the workflow performance artifact's attempt number and job queue timestamps when investigating latency, and retain failed attempts when joining build, deployment and reconciliation runs.
A successful recovery rerun against an already-serving revision does not replace the original delivery duration or its failed serving deadline.
Frontend deployment records `publication-wait` until `production` contains the promoted infrastructure commit, then starts the 60-second exact served-revision check.
Publication has an eight-minute budget within the existing ten-minute deployment job; total delivery latency includes both stages, and divergent production history fails immediately.
Measured results and scope limits are recorded in [CI performance](ci-performance.md#execution-measurements).
