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
| Hourly verification | `infra-verification-request.timer` on `fredrir-06` → `reconcile.yml` with `verify=true`, `repair=true` → `infra reconcile verify --deep --report REPORT` | Read-only verification and check-mode comparison of OpenTofu and each host playbook; a report listing differences dispatches one full reconciliation of `main` |
| On-demand verification | `gh workflow run reconcile.yml --ref main -f verify=true` | The hourly verification on demand; differences are reported without dispatching a reconciliation unless `-f repair=true` |
| Verification | `infra reconcile verify` | Reconciliation lock and recorded status, exact Flux revision, observed generations, Helm readiness, host checks, Grafana configuration and HTTP health, frontend revision; every comparison runs when another fails |
| Status | `infra reconcile status` | Desired revision, successfully applied revision, failing stage and stage durations |

| Verification report | Value |
| --- | --- |
| Differences | OpenTofu plan changes, host tasks changed in check mode, runner drift, Flux objects that differ from or have not applied the published revision, unpublished deploying changes, an incomplete or failed recorded reconciliation, a published revision not on `main`, live rulesets that differ from `.github/*-ruleset.json` |
| Errors | Unreachable hosts, failed host tasks, playbooks that could not be compared, API failures, readiness, timeouts, suspended Flux objects, replica counts a manifest does not declare |
| Held or unreadable reconciliation lock | Error; comparisons skipped, or discarded when the lock is taken during them; a held lock exits 75 |
| No state bucket access | Error; comparisons skipped |
| Ten-minute budget exceeded | Error; comparisons discarded; report and log written |
| Unpublished deploying changes | Difference; comparisons skipped |
| Repair not dispatched | `repair=false`; only `rulesets` differences; push reconciliation on `main` with an incomplete `reconcile / apply` job; latest `github-actions[bot]` dispatch for the commit ended in `failure`, `timed_out` or `startup_failure`, or started within six hours and was not cancelled |
| Repair cap reset | New commit on `main` |

| Verification trigger | Value |
| --- | --- |
| Host / role | `fredrir-06` (`external`) / `ansible/roles/verification_trigger` |
| Timer | `infra-verification-request.timer`: `OnCalendar=hourly`, `Persistent=true`, `RandomizedDelaySec=5min` |
| Service | `infra-verification-request.service`: oneshot `infra reconcile request-verification`, `DynamicUser=yes`, IPv4/IPv6 sockets only, read-only system, `TimeoutStartSec=160min`; runs never overlap, and an hour that elapses during a run starts one run after it completes |
| Binary | `/usr/local/bin/infra` from `build/cli-release.json`; a CLI release also runs `external.yml --tags=infra_binary` |
| Credentials | Root `0600` `/etc/infra-verification/github-app.pem` and `gatus-token`, delivered through `LoadCredential=github-app-key` and `gatus-token` |
| Token | Installation token restricted to `infra` with `actions: write` |
| Dispatch | `reconcile.yml` at `main`, `verify=true`, `repair=true`, actor `fredrir-infra-verification[bot]` |
| Wait | Polls the dispatched run every 30 s with ETag revalidation; honors `Retry-After` and `X-RateLimit-Reset`; deadline 150 minutes |
| Heartbeat | Gatus `reconciliation_verification`: `success=true` for `success`; `success=false` with the run URL and conclusion for `failure`, `timed_out`, `startup_failure` or the deadline; none for `cancelled` or a stopped unit |
| Failed dispatch | Unit `failed`; journal `infra: dispatch reconcile.yml in fredrir/infra at main: ERROR`; no run and no heartbeat; the next hour retries |

| Owner notification | Value |
| --- | --- |
| Failed, timed out or unfinished verification run | Email `reconciliation/verification: Alert triggered` on the report; Gatus result error names the run URL and conclusion |
| No report for 3 hours | Same email; dispatch failures or a stopped timer |
| `fredrir-06` or Gatus unreachable for 10 minutes | Alertmanager email `IndependentMonitorDown` from the cluster's scrape of `100.86.241.75:8080/metrics`; Gatus cannot report its own host |
| Next successful run | Email `reconciliation/verification: Alert resolved` |
| Superseded run | No email |
| Deployment scope | Role and Gatus changes run `external.yml --tags=gatus,verification_trigger` |

```sh
ssh -o HostKeyAlias=fredrir-06 root@100.86.241.75 systemctl list-timers infra-verification-request.timer --no-pager
ssh -o HostKeyAlias=fredrir-06 root@100.86.241.75 journalctl -u infra-verification-request.service --no-pager -n 20
ssh -o HostKeyAlias=fredrir-06 root@100.86.241.75 systemctl start infra-verification-request.service
```

| GitHub App | Value |
| --- | --- |
| Name | `fredrir-infra-verification` |
| Permissions | Actions: read and write; Metadata: read |
| Webhook | Disabled |
| Installation | `fredrir/infra` only |
| App / installation ID | `5077572` / `164918469`; `fredrir-06` in `ansible/inventory/production.yml` |
| Private key | `ansible/roles/verification_trigger/files/github-app.sops.yaml`, field `private_key`; recipients in [Secrets](Secrets.md) |
| Rotation | Generate a key in the App settings; replace `private_key`; reconcile; delete the previous key |
| Heartbeat token | `ansible/roles/verification_trigger/files/heartbeat.sops.yaml`, field `token`; equal to the `reconciliation/verification` token in `ansible/roles/gatus/files/config.sops.yaml` |

```sh
sops set ansible/roles/verification_trigger/files/github-app.sops.yaml '["private_key"]' "$(jq -Rs . < NEW_KEY.pem)"
```

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
| Cross-client lease | Conditional S3 writes to `reconciliation/production/lock.json` |
| Lease TTL / renewal | 10 minutes / every 3 minutes, each attempt bounded to 30 seconds; a taken-over or unrenewable lease cancels the run, interrupting its current step, and records no further status |
| Lease wait | `infra reconcile apply --wait DURATION`; default fails fast; CI waits 11 minutes to outlast an abandoned lease |
| Run deadline / apply job timeout | 90 minutes / 120 minutes |
| Exit codes | 0 success; 75 retry: lease held (`apply`, `verify`), or `main` advanced beyond root Markdown, `docs/**/*.md` and `build/evidence/*.json`; 1 failure |
| Retry in CI | Succeeds only while a newer push reconciliation of `main` has not completed its apply job |
| Provenance gate | Before any checkout tooling runs, including drift verification, each commit after the applied revision is SSH-signed by a key in `keys/admin_keys` at the applied revision, is a deployment, or is named by a later owner-signed `Provenance-Acknowledged: SHA` trailer |
| Deployment commit | Reproduces the `infra ci deploy` rewrite of its parent byte for byte, and its image digest is attested for the receipt's revision, run and attempt by the mapped repository's approved `build-image.yml` revision |
| Attestation tools | `gh attestation verify` for public repositories, `cosign verify` for private ones; both on `PATH` |
| Attestation credentials | `PROVENANCE_TOKEN`, a GitHub token with `packages: read`; unset uses the ambient `gh` login and Docker configuration; removed from the environment before any child process; also reads rulesets during verification, anonymously when unset |
| Owner-signed | Authenticates the owner's workstation key: any process on that workstation can sign; the gate blocks remote writers (Octo STS, stolen deploy tokens, other machines), not a compromised workstation |
| Provenance base | Applied revision; none or not an ancestor refuses; `--provenance-base SHA` overrides and must precede `HEAD`; base, revision and override are recorded in `status.json` |
| Standalone gate | `infra reconcile provenance [--provenance-base SHA] [--report PATH]`; reads the applied revision without the lease |
| CI gate | The `Verify commit provenance` step in `reconcile-job.yml` runs a release CLI pinned in that workflow before any composite action, checkout-built CLI or checkout tooling, for apply, drift verification and verification; workflow files need the `workflows` permission, which Octo STS lacks |
| CI gate release | `GATE_RELEASE` and `GATE_SHA256` in `reconcile-job.yml`; a changed pin is a workflow change |
| Workstation apply | Run `infra reconcile provenance` with an installed release CLI before building or running anything from the checkout; a CLI built from an unverified checkout can skip its own gate |
| OpenTofu locking | S3 lockfile retained; acquisition timeout 5 minutes |
| Failed verification | Old routes retained; applied revision unchanged |
| Failed retirement | Applied revision unchanged; the next attempt reads actual OpenTofu state |
| Reports | GitHub job summary and `reconciliation-RUN_ID-ATTEMPT` artifact |
| Sensitive plans | Private temporary directory; never uploaded as workflow artifacts |
| Fork pull requests | Declaration validation only; live-plan check fails until changes are on a trusted repository branch |

```sh
infra reconcile provenance
go build -o .infra/bin/infra ./cmd/infra
.infra/bin/infra reconcile plan --base BASE_SHA
doppler run --project infra --config prd_reconciliation_apply --only-secrets PUBLISHER_APP_PRIVATE_KEY -- .infra/bin/infra reconcile apply --report .infra/reconciliation/status.json
.infra/bin/infra reconcile status
.infra/bin/infra reconcile verify
doppler run --project infra --config prd_reconciliation_apply --only-secrets PUBLISHER_APP_PRIVATE_KEY -- .infra/bin/infra reconcile apply --full
```

### Publishing

| Publishing | Value |
| --- | --- |
| Identity | GitHub App `fredrir-infra-publisher`: `contents: write`, `metadata: read`; `fredrir/infra` only; no webhook; App and installation IDs in `build/publisher.json` |
| Private key | `PUBLISHER_APP_PRIVATE_KEY`, Doppler `prd_reconciliation_apply`; CI passes it only to the apply path of the `reconcile` step, never to the gate or verification; removed from the environment before any child process; `apply` fails without it |
| Token | Minted after the `main` checks; repository `infra`, `contents: write`; revoked after the push |
| Push | `git push --no-verify https://github.com/fredrir/infra.git REVISION:refs/heads/production`; never forced; token only in the push child's environment through an inline credential helper; never argv, `.git/config` or logs; global, system and other helper configuration ignored |
| Apply job token | `contents: read`; checkout persists no credentials |
| [`production`](../.github/production-ruleset.json) ruleset | Creation and update of `refs/heads/production`; bypass: publisher App only |
| [`production-history`](../.github/production-history-ruleset.json) ruleset | Deletion and non-fast-forward of `refs/heads/production`; no bypass |
| Two rulesets | A bypass actor skips every rule of the ruleset listing it; the history ruleset holds the publisher to fast-forwards |
| Administrators | Not bypass actors; their pushes to `production` are rejected |
| Drift | Hourly verification: a published revision not on `main` is a `revision` difference; live rulesets that differ from their declaration are `rulesets` differences and dispatch no repair |
| Ruleset read | `metadata: read` (`github.token`); bypass actors are returned only with write access to the ruleset (repository administration), so verification compares them only in administrator runs and the qualification |
| Break-glass | An administrator publishes as the App with the Doppler key; fallback: disable `production`, push, re-enable |
| Rollback | Disable both rulesets |

| `infra dev qualify publishing` | Value |
| --- | --- |
| Requires | `GH_TOKEN` of a repository administrator with the `workflow` scope; `PUBLISHER_APP_PRIVATE_KEY` |
| Applies | Both declared rulesets, then adds `refs/heads/production-canary` to them |
| Rejected | Administrator fast-forward; `GITHUB_TOKEN` fast-forward with `contents: write` from a throwaway push workflow on `production-canary-probe`; publisher force-push; publisher deletion |
| Accepted | Publisher fast-forward |
| Cleanup | Declared rulesets without the canary; `production-canary` and `production-canary-probe` deleted; also on start, so reruns are safe |
| Result | Live rulesets equal their declarations, bypass actors included |

```sh
gh auth refresh --scopes workflow
GH_TOKEN="$(gh auth token)" doppler run --project infra --config prd_reconciliation_apply --only-secrets PUBLISHER_APP_PRIVATE_KEY -- go run ./cmd/infra dev qualify publishing -- -timeout=30m
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
| `SOPS_AGE_KEY` | Unset | Decrypt host monitoring, verification trigger and backup credentials |
| `RUNNER_APP_ID`, `RUNNER_APP_PRIVATE_KEY` | Unset | Existing runner GitHub App; mint a short-lived installation token with repository administration permission |
| `PUBLISHER_APP_PRIVATE_KEY` | Unset | [Publisher App](#publishing) private key |

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
| CI SOPS recipient | Added only to host monitoring, verification trigger and backup secret files |
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
| Newer merge supersedes a queued run | Reconcile current `main`; runs superseded by reconciled changes cannot publish; a retry fails unless a newer push run has yet to complete its apply |
| On-demand or hourly verification supersedes a run queued in the `infrastructure-production` concurrency group | When the superseded run carried deploying changes, the superseding hourly verification, or the next hourly one after an on-demand verification, reports the unapplied revision and dispatches a full reconciliation; dispatch `verify=true` when no apply is queued |
| Failed apply or verification | Rerun the workflow or run `infra reconcile apply --full` from a clean current `main` checkout |
| Unverified commits | Revert unwanted changes; push an owner-signed commit with one `Provenance-Acknowledged: SHA` trailer per listed commit |
| Published revision not on `main` | Publisher key compromised: rotate it in the App settings and Doppler; disable both rulesets; `git push --force origin APPLIED_SHA:refs/heads/production`; rerun `infra dev qualify publishing`; `infra reconcile apply --full` |
| Ruleset differences | Rerun `infra dev qualify publishing`; it restores the declared rulesets |
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
| Retirement       | Undeclared runners lose their labels, drain like replacements and are deregistered before their root is removed |
| Job routing      | `dagger-amd64` or `infra-trusted`; `self-hosted`, `Linux` and `X64` stay during a drain                   |

A changed digest for an unchanged version is not proposed; investigate it before editing the pin.

| Missing ARC listener | Value |
| --- | --- |
| Symptom | Jobs for one scale-set label stay queued; `AutoscalingRunnerSet` phase `Pending`; controller log `waiting for the running and pending runners to finish`; verification reports `has no running listener` |
| Cause | A release change outside the runner template deletes the listener, and `updateStrategy: eventual` waits for a warm idle runner that cannot finish without one |
| Prevention | Warm scale sets carry a hash of their Helm values file in the runner template annotation `infra.fredrir.com/values`, so ARC replaces idle runners |
| Remediation | `kubectl -n <namespace> get ephemeralrunnersets`, then `kubectl -n <namespace> patch ephemeralrunnerset <name> --type=merge -p '{"spec":{"replicas":0}}'`; ARC recreates the listener |

The Rust reusable workflow accepts `build-output-cache: false` to skip target archive restore and publication while retaining compiler `sccache`.
`nsql` uses this setting because the measured archive refresh cost exceeded the compilation saved; identical-output reruns benefited from archive reuse.
Compare `rust-cache-restore`, `rust-preparation`, `rust-cache-save` and total job duration before changing this setting.
The aggregate check budget remains ten seconds, and cache publication still requires a successful trusted build.

Use the workflow performance artifact's attempt number and job queue timestamps when investigating latency, and retain failed attempts when joining build, deployment and reconciliation runs.
A successful recovery rerun against an already-serving revision does not replace the original delivery duration or its failed serving deadline.
Frontend deployment records `publication-wait` until `production` contains the promoted infrastructure commit, then starts the 60-second exact served-revision check.
Publication has an eight-minute budget within the existing ten-minute deployment job; total delivery latency includes both stages, and divergent production history fails immediately.
Measured results and scope limits are recorded in [CI performance](ci-performance.md#execution-measurements).
