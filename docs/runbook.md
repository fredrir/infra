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
| Pull request | `infra ci prepare-validation`, `infra ci validate`, `infra reconcile plan --base BASE_SHA` | Affected declarations, OpenTofu tests, expansion and final-state plans, Flux rendering, Ansible task lists |
| Merge to `main` | `infra reconcile apply` | Fresh plan for the exact checkout, gated on OpenTofu tests; apply against the last successful revision |
| Hourly verification | `infra-verification-request.timer` on `fredrir-06` → `reconcile.yml` with `verify=true`, `repair=true` → `infra reconcile verify --scope=full --report REPORT` | Read-only verification and check-mode comparison of OpenTofu and each host playbook; a report listing differences dispatches one full reconciliation of `main` |
| On-demand verification | `gh workflow run reconcile.yml --ref main -f verify=true` | The hourly verification on demand; differences are reported without dispatching a reconciliation unless `-f repair=true` |
| Cloud verification | `infra reconcile verify --scope=cloud` | Reconciliation lock and recorded status, rulesets, exact Flux revision, observed generations, Helm readiness, runner listeners and registrations, Grafana configuration and HTTP health, frontend revision, OpenTofu plan comparison; no host access; every comparison runs when another fails |
| Full verification | `infra reconcile verify --scope=full` | Cloud verification plus host checks and each host playbook compared in check mode; root-equivalent on every host |
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
| Pinned CLI | Must accept `request-verification --heartbeat`; a unit change that passes a new flag lands together with a `build/cli-release.json` pin of a release that has it |
| Credentials | Root `0600` ciphertext `/etc/infra-verification/credentials.sops.yaml`; decrypted at start into the unit's runtime directory with the [host key](Secrets.md#host-scoped-secrets) |
| Token | Installation token restricted to `infra` with `actions: write` |
| Dispatch | `reconcile.yml` at `main`, `verify=true`, `repair=true`, actor `fredrir-infra-verification[bot]` |
| Wait | Polls the dispatched run every 30 s with ETag revalidation; honors `Retry-After` and `X-RateLimit-Reset`; deadline 150 minutes |
| Heartbeat | Gatus `reconciliation_deep` (`--heartbeat=reconciliation_deep`): `success=true` for `success`; `success=false` with the run URL and conclusion for `failure`, `timed_out`, `startup_failure` or the deadline; none for `cancelled` or a stopped unit |
| Failed dispatch | Unit `failed`; journal `infra: dispatch reconcile.yml in fredrir/infra at main: ERROR`; no run and no heartbeat; the next hour retries |

| Owner notification | Value |
| --- | --- |
| Failed, timed out or unfinished verification run | Email `reconciliation/deep: Alert triggered` on the report; Gatus result error names the run URL and conclusion |
| No report for 3 hours | Same email; dispatch failures or a stopped timer |
| `fredrir-06` or Gatus unreachable for 10 minutes | Alertmanager email `IndependentMonitorDown` from the cluster's scrape of `100.86.241.75:8080/metrics`; Gatus cannot report its own host |
| Next successful run | Email `reconciliation/deep: Alert resolved` |
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
| Private key | `ansible/roles/verification_trigger/files/credentials.sops.yaml`, field `private_key`; recipients in [Secrets](Secrets.md) |
| Rotation | Generate a key in the App settings; replace `private_key`; reconcile; delete the previous key |
| Heartbeat token | `ansible/roles/verification_trigger/files/credentials.sops.yaml`, field `token`; equal to `GATUS_TOKEN_RECONCILIATION_DEEP` in `ansible/roles/gatus/files/secrets.sops.yaml` |

```sh
jq -Rs . < NEW_KEY.pem | sops set --value-stdin ansible/roles/verification_trigger/files/credentials.sops.yaml '["private_key"]'
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
| Cross-client lease | Conditional S3 writes and deletes of `reconciliation/production/lock.json`; after a 409, 412, 5xx or lost response the lease is read back: its own body, or its absence after release, confirms the request, an unchanged lease is reissued once, anything else counts as taken over |
| S3 retries | Dial, TLS, reset, EOF, 5xx and `SlowDown` failures: 3 attempts with jittered exponential backoff; a conditional request that may have reached S3 is settled instead of replayed |
| Lease TTL / renewal | 10 minutes / every 3 minutes, each attempt bounded to 30 seconds; a taken-over or unrenewable lease cancels the run, interrupting its current step, and records no further status |
| Lease wait | `infra reconcile apply --wait DURATION`; default fails fast; CI waits 11 minutes to outlast an abandoned lease |
| Run deadline / apply job timeout | 90 minutes / 120 minutes |
| Exit codes | 0 success; 75 retry: lease held (`apply`, `verify`), or `main` advanced beyond root Markdown, `docs/**/*.md` and `build/evidence/*.json`; 1 failure |
| Retry in CI | Succeeds only while a newer push reconciliation of `main` has not completed its apply job |
| Provenance gate | Before any checkout tooling runs, including drift verification, each commit after the applied revision is SSH-signed by a key in `keys/admin_keys` at the applied revision, is a deployment, belongs to a reviewed merge, or is named by a later owner-signed `Provenance-Acknowledged: SHA` trailer |
| Deployment commit | Reproduces the `infra ci deploy` rewrite of its parent byte for byte, and its image digest is attested for the receipt's revision, run and attempt by the mapped repository's approved `build-image.yml` revision |
| Reviewed merge | A pull request merged into `main` whose head was approved before the merge by a person other than its author who is a `@user` owner of every landed path in `.github/CODEOWNERS` at the applied revision |
| Reviewed merge commits | Merge or squash commit: signed by `keys/github-web-flow.asc` at the applied revision; a merge commit also covers the pull request's commits. Rebase merge: unsigned; the linear chain ending at its merge commit is as long as the pull request's non-merge, non-empty commits |
| Reviewed merge content | The landed tree equals `merge-tree` of the approved head onto the commit below the merge, squash or rebased chain; the head is fetched by SHA when absent |
| Reviewed merge failures | Any API, fetch or merge failure leaves the commit unverified; owner-authored pull requests have no approval and need an acknowledgement or an owner-signed push |
| Attestation tools | `gh attestation verify` for public repositories, `cosign verify` for private ones; both on `PATH` |
| Attestation credentials | `PROVENANCE_TOKEN`, a GitHub token with `packages: read` and `pull-requests: read`; unset uses the ambient `gh` login and Docker configuration; removed from the environment before any child process; also reads pull requests and, during verification, rulesets, anonymously when unset |
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
publisher_key() { key="$(mktemp)" && doppler secrets get PUBLISHER_APP_PRIVATE_KEY --project infra --config prd_reconciliation_apply --plain > "$key" && printf '%s\n' "$key"; }
PUBLISHER_APP_PRIVATE_KEY_FILE="$(publisher_key)" .infra/bin/infra reconcile apply --report .infra/reconciliation/status.json
.infra/bin/infra reconcile status
.infra/bin/infra reconcile verify --scope=full
PUBLISHER_APP_PRIVATE_KEY_FILE="$(publisher_key)" .infra/bin/infra reconcile apply --full
```

### Publishing

| Publishing | Value |
| --- | --- |
| Identity | GitHub App `fredrir-infra-publisher`: `contents: write`, `metadata: read`; `fredrir/infra` only; no webhook; App and installation IDs in `build/publisher.json` |
| Private key | `PUBLISHER_APP_PRIVATE_KEY`, Doppler `prd_reconciliation_apply`; never in a process environment |
| Key delivery | `PUBLISHER_APP_PRIVATE_KEY_FILE`: a regular file readable only by its owner; the CLI reads and deletes it before any child process; `apply` fails without it; CI writes `$RUNNER_TEMP/publisher/key.pem` with `umask 077` in a separate step for apply runs only, never for the gate or verification; session cleanup removes it |
| Key rotation | Generate a key in the App settings; `doppler secrets set PUBLISHER_APP_PRIVATE_KEY --project infra --config prd_reconciliation_apply < NEW_KEY.pem`; delete the previous key |
| Token | Minted after the `main` checks; repository `infra`, `contents: write`; revoked after the push |
| Push | `git push --no-verify https://github.com/fredrir/infra.git REVISION:refs/heads/production`; never forced; token only in the push child's environment through an inline credential helper; never argv, `.git/config` or logs; global, system and other helper configuration ignored; `GIT_TRACE*` and `GIT_CURL_VERBOSE` removed, `GIT_TRACE_REDACT=1`; `::add-mask::` under GitHub Actions |
| Apply job token | `contents: read`; checkout persists no credentials |
| Runner token | `GH_TOKEN` of the runner App (repository administration on `build/runners.json` repositories, `infra` included); removed from the CLI's environment at start; passed only to runner-fleet API calls and `build-runners.yml` and `reconcile.yml` runs; OpenTofu, Kubernetes tools, other playbooks and check-mode runs never receive it |
| [`production`](../.github/production-ruleset.json) ruleset | Creation and update of `refs/heads/production`; bypass: publisher App only |
| [`production-history`](../.github/production-history-ruleset.json) ruleset | Deletion and non-fast-forward of `refs/heads/production`; no bypass |
| Two rulesets | A bypass actor skips every rule of the ruleset listing it; the history ruleset holds the publisher to fast-forwards |
| Administrators | Not bypass actors; their pushes to `production` are rejected |
| Drift | Hourly verification: a published revision not on `main` is a `revision` difference; live rulesets that differ from their declaration are `rulesets` differences and dispatch no repair |
| Ruleset read | `metadata: read` (`github.token`); bypass actors are returned only with write access to the ruleset (repository administration: write); the verification runner token (`administration: read`) receives none either, so verification compares them only in administrator runs and the qualification |
| Break-glass | An administrator publishes as the App with the Doppler key through `PUBLISHER_APP_PRIVATE_KEY_FILE`; no administrator bypass |
| Rollback | Disable both rulesets |

| `infra dev qualify publishing` | Value |
| --- | --- |
| Requires | `GH_TOKEN` of a repository administrator with the `workflow` scope; `PUBLISHER_APP_PRIVATE_KEY_FILE`, consumed like the CLI's |
| Applies | Both declared rulesets, then adds `refs/heads/production-canary` to them |
| Rejected | Administrator fast-forward; `GITHUB_TOKEN` fast-forward with `contents: write` from a throwaway push workflow on `production-canary-probe`; publisher force-push; publisher deletion |
| Accepted | Publisher fast-forward |
| Cleanup | Declared rulesets without the canary; `production-canary` and `production-canary-probe` deleted; also on start, so reruns are safe |
| Result | Live rulesets equal their declarations, bypass actors included |

```sh
gh auth refresh --scopes workflow
publisher_key() { key="$(mktemp)" && doppler secrets get PUBLISHER_APP_PRIVATE_KEY --project infra --config prd_reconciliation_apply --plain > "$key" && printf '%s\n' "$key"; }
GH_TOKEN="$(gh auth token)" PUBLISHER_APP_PRIVATE_KEY_FILE="$(publisher_key)" go run ./cmd/infra dev qualify publishing -- -timeout=30m
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
| `RUNNER_APP_ID`, `RUNNER_APP_PRIVATE_KEY` | Unset | Runner GitHub App; applies mint a short-lived installation token with repository administration write |
| `OBSERVER_APP_ID`, `OBSERVER_APP_PRIVATE_KEY` | Unset | Observer GitHub App; verifications mint a short-lived installation token with repository administration read |
| `PUBLISHER_APP_PRIVATE_KEY` | Unset | [Publisher App](#publishing) private key; delivered to the engine as a file |

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
| AWS verify identity | `/automation/infra-reconciliation-verify`: the plan policy without the OpenTofu lock, plus `s3:PutObject` on `reconciliation/production/runs/*`; every request from an address other than `reconciler_ipv4` is denied; declared only while `reconciler_ipv4` is set in `tofu/production.tfvars.json` |
| Run report retention | `reconciliation/production/runs/` objects expire after 30 days; noncurrent versions under `reconciliation/production/` after 7 days |
| AWS workload boundary | `policy/boundary/infra-workload-boundary` on every OpenTofu-managed IAM user outside `/automation/`; dataset S3 objects except `tofu-state/` and `reconciliation/`, SES from the alert sender; the apply identity cannot edit, remove or bypass it |
| Kubernetes identities | `platform/components/policy/reconciliation.yaml` |
| Kubernetes tokens | Controller-populated `infrastructure-plan-credentials`, `infrastructure-apply-credentials` and `infrastructure-verify-credentials` Secrets in `flux-system` |
| Kubernetes API address | Reachable control-plane Tailnet address with a matching certificate; include its CA in each kubeconfig |
| Kubernetes verify scope | Read Flux resources, workloads, runner sets, listener pods and artifacts; no writes |
| Kubernetes apply scope | Verify scope; patch reconciliation annotations; admission rejects spec changes |
| Tailnet plan scope | Control-plane API only |
| Tailnet apply scope | Control-plane API and SSH to managed hosts |
| Tailnet reconciler scope | `tag:infra-reconciler`: control-plane API and Gatus heartbeats; reached only by Macie and Archie on SSH |
| Host SSH key | `ansible/files/reconciliation.pub`; maintained by `ansible/reconciliation-identity.yml` |
| Provider-policy, workload-boundary or bucket-lifecycle changes, revoked credentials | Administrator repair required |

```sh
doppler configs tokens create github-reconciliation-plan --project infra --config prd_reconciliation_plan --access read --plain | gh secret set DOPPLER_TOKEN --env infrastructure-plan
doppler configs tokens create github-reconciliation-apply --project infra --config prd_reconciliation_apply --access read --plain | gh secret set DOPPLER_TOKEN --env infrastructure-apply
gh workflow run reconcile.yml --ref main
```

### CI SOPS key retirement

The retired CI apply recipient `age1jm6xj8qlmfjlhw0vdseaaqkpt3mqj3yl0upx3smwutsaghcq6pesvrka2t` still decrypts every earlier revision of `ansible/roles/gatus/files/config.sops.yaml`, `ansible/roles/verification_trigger/files/github-app.sops.yaml`, `ansible/roles/verification_trigger/files/heartbeat.sops.yaml` and `platform/components/backups/backup.secret.sops.yaml`.

| Order | Action | Check |
| --- | --- | --- |
| 1 | After a green apply without it, delete `SOPS_AGE_KEY` from Doppler `infra/prd_reconciliation_apply` | `doppler secrets get SOPS_AGE_KEY --project infra --config prd_reconciliation_apply` fails; the next apply succeeds |
| 2 | Verification heartbeat, then each backup heartbeat, one commit per token | [Rotate](Secrets.md#host-scoped-secrets) |
| 3 | Verification App key | GitHub App settings |
| 4 | SMTP | Administrator IAM |
| 5 | Control backup access key | Administrator IAM |
| 6 | Control repository password | `restic key remove` last |

### Recovery

| Failure | Recovery |
| --- | --- |
| Missing credentials or private connectivity | Repair the environment identity, host enrollment or Tailnet policy; rerun the workflow |
| Newer merge supersedes a queued run | Reconcile current `main`; runs superseded by reconciled changes cannot publish; a retry fails unless a newer push run has yet to complete its apply |
| On-demand or hourly verification supersedes a run queued in the `infrastructure-production` concurrency group | When the superseded run carried deploying changes, the superseding hourly verification, or the next hourly one after an on-demand verification, reports the unapplied revision and dispatches a full reconciliation; dispatch `verify=true` when no apply is queued |
| Failed apply or verification | Rerun the workflow or run `infra reconcile apply --full` from a clean current `main` checkout |
| Unverified commits | Revert unwanted changes; push an owner-signed commit with one `Provenance-Acknowledged: SHA` trailer per listed commit |
| Published revision not on `main` | Publisher fast-forward to a commit off `main`: rotate the publisher key first. `main` force-pushed by a `main` bypass actor (repository admin role, Octo STS `801323`): no key rotation. Either way, rewind as below |
| Rewinding `production` | `production-history` has no bypass, so nobody can force-push `production` while it is enforced; an administrator disables both rulesets, force-pushes the fork point with `main`, re-enables both, and applies from that base |
| Ruleset differences | Rerun `infra dev qualify publishing`; it restores the declared rulesets |
| Process terminated without lock cleanup | Hourly verification reports the held lock until its recorded expiry, then the incomplete reconciliation as a difference |
| Remaining OpenTofu drift | Inspect the final plan; nonzero drift keeps the run failed |
| Image publication succeeds | Check the separate reconciliation workflow for production readiness |

```sh
git fetch origin main production
base="$(git merge-base origin/production origin/main)"
ruleset() { gh api repos/fredrir/infra/rulesets --jq ".[] | select(.name == \"$1\") | .id"; }
gh api --method PUT "repos/fredrir/infra/rulesets/$(ruleset production-history)" -f enforcement=disabled
gh api --method PUT "repos/fredrir/infra/rulesets/$(ruleset production)" -f enforcement=disabled
git push --force origin "$base:refs/heads/production"
gh api --method PUT "repos/fredrir/infra/rulesets/$(ruleset production)" -f enforcement=active
gh api --method PUT "repos/fredrir/infra/rulesets/$(ruleset production-history)" -f enforcement=active
PUBLISHER_APP_PRIVATE_KEY_FILE="$(publisher_key)" .infra/bin/infra reconcile apply --full --provenance-base "$base"
```

Do not remove an active reconciliation or OpenTofu lock while its writer is running.

## Volatile workers

| Setting | Value |
| --- | --- |
| Hosts | Inventory group `volatile`, also in `agent`: `fredrir-10` |
| Reconciliation | `ansible/volatile.yml`, linear strategy, run separately after the fleet is verified; fleet plays target `…:!volatile` |
| Failing or unreachable host | Recorded as `volatile_failure` in the reconciliation status; the run completes and requests no recovery |
| Deep verification | `volatile.yml --check` beside the fleet; results under `degraded`; outcome and exit status unchanged |
| Unenrolled host | `tailscale_ip: null`; skipped |
| Node admission | `node-registration` policy: kubelets register only inventory nodes; volatile nodes must carry the taint; nodes cannot remove taints |
| Shared flannel identity | Every agent holds the `system:k3s-controller` certificate, which may patch any node's status (k3s writes flannel annotations with it, not the kubelet identity). `node-flannel-writer` lets it change only flannel annotations and the `NetworkUnavailable` condition: backend type and data are write-once, public addresses must be the node's own and may not be removed, and labels, spec, owners, finalizers, addresses, capacity, allocatable, node info, daemon endpoints, images, runtime handlers, features and other conditions are pinned |
| First-value race | `node-flannel-writer` accepts any first backend value while it is unset, which is the state at every registration and after every admin clear; the shared identity could win that race. Mitigation is operational, not in-policy: fredrir-10 is enrolled only after the fleet is on verified WireGuard (`volatile.yml` gate), and every later clear cuts fredrir-10 off the API first. The k3s role fails closed rather than clear or first-set a non-volatile node's flannel backend while any node carries the volatile taint or label (the taint survives a delete-and-re-register because `node-registration` requires it), or while any inventory `volatile` host has a `tailscale_ip` unless the run passes `-e volatile_api_cut_off=true`, so an automated repair, rollback, or fleet-node (re)registration cannot reopen the race. The inventory check covers a volatile host whose Node was deleted but which still holds its `system:k3s-controller` certificate |
| Kubelet pod writes | `node-no-static-pods` denies pod creation by nodes; NodeRestriction already refuses label changes through `pods/status` and status writes to another node's pods |
| Join credential | Per-node `k3s token create --ttl 30m`, deleted after join; the k3s role refuses the shared agent token and fails if any file under `/var/lib/rancher` matches its checksum; a joined agent keeps working across restarts and token deletion |
| Scheduling | Taint `node-restriction.kubernetes.io/volatile=true:NoSchedule`, applied before labels `gvisor`, `volatile`, `infra.fredrir.com/ci-slots=3`; no `critical` or `stateful` |
| Critical controllers | Flux, ARC controller and listeners, ci-slots: required `node-restriction.kubernetes.io/critical=true` |
| CI pools | `check-amd64`, `rust-amd64` and `rust-pr-amd64` tolerate and prefer it and leave 30 s after not-ready or unreachable; `rust-release-amd64` and `rust-tag-amd64` never run there |
| Monitoring | node-exporter only; Alloy stays off because pod-log reads cannot be scoped to one node. fredrir-10 is dropped from the fleet kubelet and node-exporter ServiceMonitors and scraped by its own `kubelet-volatile` (sampleLimit 5000, per-container cAdvisor series dropped) and `node-exporter-volatile` (sampleLimit 3000) monitors, so it cannot grow the Prometheus head. The volatile monitors set `honorLabels: false` and `honorTimestamps: false` so fredrir-10 cannot forge another node's labels or backdate samples |
| Volatile series labels | With `honorLabels: false`, fredrir-10's kubelet and cAdvisor series carry the target namespace `kube-system`, and their real `namespace` and `pod` move to `exported_namespace` and `exported_pod`, so namespace-keyed alerts do not attribute fredrir-10 series to the workload's namespace |
| Kubelet scrape credential | The volatile kubelet monitors present a 1 h projected token (`bearerTokenFile`), not the chart's non-expiring `monitoring-prometheus-token`, so a hostile kubelet capturing it can replay it against the API for at most its lifetime; the token is the Prometheus pod's own `monitoring-prometheus` identity (nodes, nodes/metrics, services, endpoints, pods, endpointslices, ingresses read; no nodes/proxy, no writes). A dedicated least-privilege ServiceAccount is not achievable through the operator: projected tokens mint only for the pod's ServiceAccount, and a kubelet-only audience fails the kubelet's TokenReview against the cluster api-audiences, so the pod identity with a short lifetime is the least exposure. `monitoring-prometheus-token` is created by the kube-prometheus-stack chart (`prometheus.serviceAccount.createTokenSecret`) for the fleet kubelet monitor, not a leftover |
| Readiness waits | `verify.yml` and `maintenance.yml` exclude `node-restriction.kubernetes.io/volatile`; `maintenance.yml` never targets volatile hosts, which take unattended patching |
| Tailnet tag | `tag:platform-volatile`: 6443 to the control-plane routes, 8472/udp with the fleet; the fleet reaches its 9100 and 10250; SSH from Macie, Archie and `tag:infra-apply` |
| Inbound access | Tailnet; NTNU VPN (`~/ntnu-proxy`, `10.50.0.0/16`) is owner-only break-glass SSH with keys only, never used by the fleet |
| Container engines | Docker, containerd.io, Podman and Buildah removed at takeover; only the k3s agent runs |
| Trust | NTNU controls hypervisor and network; no ProxyJump, `ForwardAgent=no`, no delegated secrets; holds its node credentials and the job credentials of its CI pools |
| Removal | Delete from `agent`, `volatile` and `node-registration`; `kubectl delete node fredrir-10`; delete `fredrir-10.node-password.k3s` |

| Data volume | Value |
| --- | --- |
| Device | `data_volume_device`; 500 GB OpenStack SSD `scsi-0QEMU_QEMU_HARDDISK_577daadf-6f2c-4cdc-8639-86108aa264b7`; partition `-part1`; ext4 label `infra-data` |
| Performance | Per-volume QoS cap (same as root): ~1000 IOPS 4k random, ~500 MB/s sequential, p99 ~50 ms; SSD class does not raise IOPS |
| Formatting | Only when `wipefs` finds no signature; foreign or whole-disk signatures fail closed |
| Mount | `srv-data.mount` at `/srv/data`; `Options=nofail`; `WantedBy=local-fs.target` |
| Layout | `data_volume_binds` (inventory-driven): `/srv/data/<name>` bound onto `/var/lib/rancher` (containerd images), `/var/lib/kubelet` (pod emptyDirs) — large mostly-sequential caches |
| Hot scratch | Latency-sensitive CI scratch (Rust `target`, sccache) belongs in a `medium: Memory` emptyDir counted against the pod's memory, not on the IOPS-capped volume; deferred until fio-on-real-volume and pod-limit headroom are confirmed (3 Rust pods at 10Gi limits vs 62 GiB RAM), so it is a measured follow-up, not yet applied |
| Consumers | `data_volume_consumers` get `RequiresMountsFor=` on the layout; running consumers restart once when a mount activates |
| Missing volume | Consumers stay stopped; nothing writes the 40 GB root |

| Transport | Value |
| --- | --- |
| Path | Direct UDP to every fleet peer through NTNU's NAT; requires inbound UDP 41641 on each peer's provider firewall; Tailscale DERP is the fallback |
| Overlay | Tailscale's bypass-marked packets never enter the pod CIDR (host firewall output chain), so tailscaled cannot pick pod addresses as endpoints |
| Measure | On fredrir-10: `tailscale status` (`CurAddr` per peer); `tailscale ping --c 20 fredrir-07`; `iperf3` to fredrir-09 over the Tailnet; Rust job `rust-cache-restore` timings |

| Flannel backend | Value |
| --- | --- |
| Declared | `k3s_flannel_backend: wireguard-native` in `ansible/roles/k3s/defaults/main.yml`; peers reach each other over the Tailnet on 51820/udp. Rolling back is `vxlan` there, applied by the same serial servers-then-agents play |
| Change | The k3s role clears a node's `backend-type`, `backend-data` and `backend-v6-data` annotations through the administrator API, then restarts it, when its published backend differs or its WireGuard key file is missing |
| Pod MTU after a backend change | Pods keep the MTU they were created with (VXLAN 1230, WireGuard 1200 over `tailscale0` 1280); once `cni0` takes the new MTU, older pods lose large packets, such as TLS handshakes with endpoints that ignore ICMP. After the change reaches every node, `kubectl rollout restart` every Deployment and delete StatefulSet and DaemonSet pods one at a time until no pod-network pod predates it |
| WireGuard key | `WIREGUARD_KEY_FILE=/var/lib/rancher/k3s/agent/flannel-wireguard.key` in the service's `flannel.conf` drop-in; it survives restarts and reboots, and every run fails when a node publishes a key not derived from its own file |
| Cut fredrir-10 off before any clear | Every clear reopens the write-once first-value race, so before clearing any node's flannel annotations first cut fredrir-10 off the API: remove `tag:platform-volatile`'s 6443 grant in `tailscale/policy.hujson` and reapply, or deauthorize fredrir-10 in the Tailnet. For an automated clear (a role repair, rollback, or fleet-node re-registration) the k3s role also refuses while any node carries the volatile taint or label, and while fredrir-10 has a `tailscale_ip` in the inventory, so stop fredrir-10's agent, then cordon, drain and `kubectl delete node fredrir-10`, and run the fleet play with `-e volatile_api_cut_off=true` to assert the 6443 cut-off is in place. Clear, confirm keys, then re-enroll and restore access |
| Repair | With fredrir-10 cut off, on a server: `k3s kubectl annotate node <node> flannel.alpha.coreos.com/backend-type- flannel.alpha.coreos.com/backend-data- flannel.alpha.coreos.com/backend-v6-data-`, then restart `k3s` or `k3s-agent` on `<node>`. If the foreign key belonged to another node, restart every other node one at a time as well: peers keep the forged peer entry until their flannel restarts |
| Rotate a key or rebuild a node | With fredrir-10 cut off, delete the key file or rejoin without it, then reconcile; the role clears the annotations and restarts the node |

### fredrir-10 activation

| Order | Owner action | Value |
| --- | --- | --- |
| 1 | Roll out nsql | Pin an infra revision whose `rust-auto-tag.yml` runs on `rust-tag-amd64`. Every SHA in nsql's `auto-tag.sts.yaml` allowlist (current and previous) must be such a revision: drop the pre-split `a57b0e1` and `d9016b2` even if one entry remains. Check each with `git show <sha>:.github/workflows/rust-auto-tag.yml \| grep runs-on` |
| 2 | Attach the OpenStack volume | The volume whose `/dev/disk/by-id` path is `data_volume_device` |
| 3 | Apply `tailscale/policy.hujson` | Adds `tag:platform-volatile` |
| 4 | Install the verified `infra` release at `/usr/local/bin/infra` over `ssh ntnu` | Release SHA-256 from the trusted build |
| 5 | Deliver the enrollment key | `infra operations enrollment create-deliver --node fredrir-10 --role volatile --host ntnu --sudo` |
| 6 | Enroll transport | Bootstrap below; prints the Tailnet IPv4 |
| 7 | Set inventory values | `tailscale_ip` |
| 8 | Trust the host key | `fredrir-10 ssh-ed25519 …` in Doppler `SSH_KNOWN_HOSTS` and the admin `known_hosts` |
| 9 | Confirm the fleet runs WireGuard (hard gate) | Every fleet node publishes `backend-type=wireguard` with its own verified key before fredrir-10 joins. `volatile.yml` refuses to enroll otherwise, and VXLAN or an unset backend must never coexist with an enrolled fredrir-10 |
| 10 | Merge; wait for `node-registration` | Flux applies the policy that declares `fredrir-10`; volatile runs report `volatile_failure` until step 12 |
| 11 | Write a per-node join token | On fredrir-07: `k3s token create --ttl 30m --description fredrir-10`; on fredrir-10: `/etc/rancher/k3s/agent-token`, root `0600` |
| 12 | First converge from Macie or Archie within the token lifetime | `ansible-playbook ansible/volatile.yml --limit fredrir-10`; installs the reconciliation key and sets the hostname |
| 13 | Delete the join token | On fredrir-07: `k3s token delete <id>`; the agent keeps its client cert and reconnects across restarts after the token is gone (verified) |

```sh
inventory=$(mktemp)
printf 'tailscale_bootstrap:\n  hosts:\n    fredrir-10:\n      ansible_host: ntnu\n      ansible_user: ubuntu\n' > "$inventory"
ansible-playbook -i "$inventory" ansible/tailscale-bootstrap.yml \
  -e '{"platform_tailscale_bootstrap_approved": true, "platform_tailscale_bootstrap_tags": ["tag:platform-volatile"], "platform_tailscale_auth_key_file": "/run/secrets/tailscale-auth-key", "platform_architecture": "amd64"}'
```
## Reconciler host

| Reconciler host | Value |
| --- | --- |
| Host / group | `fredrir-11` / `reconcilers`, outside `ubuntu` and every group a fleet play selects |
| Playbook | `ansible/reconciler.yml`, run only by an administrator; reconciliation selects nothing for it |
| Provider | Dedicated Hetzner project; `tofu/reconciler/`, state `tofu-state/reconciler.tfstate`, applied only by an administrator |
| Network | Primary IPv4; IPv6 disabled; no inbound Hetzner rules outside enrollment; tailnet `tag:infra-reconciler` |
| Timer | `infra-reconcile-verify.timer`: `OnCalendar=hourly`, `Persistent=true`, `RandomizedDelaySec=5min` |
| Service | `infra-reconcile-verify.service`: oneshot `infra reconcile run verify` as `infra-verify`; state `/var/lib/infra-verify`; the verification trigger's sandbox plus `AF_UNIX`; `TimeoutStartSec=100min`, `MemoryMax=6G` |
| Supervisor | `/usr/local/bin/infra` from `build/cli-release.json` |
| Run | Fresh clone of `production`, the revision the provenance gate admitted and the publisher App published, never `main`; a `production` revision that is not an ancestor of `main`, or a failed `main` fetch, stops the run before any build, and the former is reported as a `revision` difference; `go build` with the Go version in `build/toolchain.json`; `infra ci install-tools flux gh kubectl tofu`; observer App token for the runner repositories, revoked after the run; `infra reconcile verify --scope=cloud` |
| Run isolation | Checkout, Go caches, tools, OpenTofu providers and `HOME` live in a per-run directory; the state directory is emptied before and after each run; every Git process ignores hooks, `core.fsmonitor` and replace refs |
| Reports | `s3://llunde-pyparser-bucket/reconciliation/production/runs/<utc>-verify-<rev12>/`: `report.json`, `log.txt.zst`; journal bounded to 2 GB |
| Heartbeat | Gatus `reconciliation_verification`: `success=true` when the verification matches; otherwise the differences or the failing stage; none while a reconciliation holds the lease |
| Credentials | `ansible/roles/reconciler/files/credentials.sops.yaml`, maps `verify` and `apply`; installed as ciphertext through `host_secrets`; a root `ExecStartPre` decrypts only `verify` into the unit's runtime directory, and the supervisor deletes it once read |
| Cluster API | `https://<fredrir-07 tailnet address>:6443`; certificate authority `ansible/roles/reconciler/files/kubernetes-ca.crt` |
| Host age key | `/etc/age/host.key` from `host_secrets`; `reconciler.yml --tags host_key` generates it and prints its recipient; [host-scoped secrets](Secrets.md#host-scoped-secrets) |

| Verify credential | Source |
| --- | --- |
| `aws-access-key-id`, `aws-secret-access-key` | `tofu -chdir=tofu/reconciler output -raw verify_aws_access_key_id`, `verify_aws_secret_access_key` |
| `cloudflare-api-token` | `tofu -chdir=tofu/reconciler output -raw verify_cloudflare_api_token` |
| `hcloud-token` | Read-only token of the fleet Hetzner project |
| `platform-mail-recipient` | `PLATFORM_MAIL_RECIPIENT` |
| `kubernetes-token` | `flux-system/infrastructure-verify-credentials` |
| `observer-app-key` | `OBSERVER_APP_PRIVATE_KEY` |
| `gatus-token` | Set; equal to `GATUS_TOKEN_RECONCILIATION_VERIFICATION` in `ansible/roles/gatus/files/secrets.sops.yaml` |

```sh
credential() { jq -Rs 'rtrimstr("\n")' | sops set --value-stdin ansible/roles/reconciler/files/credentials.sops.yaml "[\"verify\"][\"$1\"]"; }
tofu -chdir=tofu/reconciler output -raw verify_aws_secret_access_key | credential aws-secret-access-key
kubectl -n flux-system get secret infrastructure-verify-credentials -o jsonpath='{.data.token}' | base64 -d | credential kubernetes-token
ssh root@fredrir-11 systemctl list-timers infra-reconcile-verify.timer --no-pager
ssh root@fredrir-11 journalctl -u infra-reconcile-verify.service --no-pager -n 50
ssh root@fredrir-11 systemctl start infra-reconcile-verify.service
aws s3 ls s3://llunde-pyparser-bucket/reconciliation/production/runs/ | tail -n 5
```

| Operation | Steps |
| --- | --- |
| Rebuild | `tofu -chdir=tofu/reconciler apply`; enroll; `ansible-playbook ansible/reconciler.yml --tags host_key`; set the new recipient as `.sops.yaml` anchor `fredrir-11`; `sops updatekeys -y ansible/roles/reconciler/files/credentials.sops.yaml`; `ansible-playbook ansible/reconciler.yml` |
| Rotation | `tofu -chdir=tofu/reconciler apply -replace=aws_iam_access_key.verify` or `-replace=cloudflare_account_token.verify`; set the new value; `ansible-playbook ansible/reconciler.yml` |
| Kubernetes token rotation | The token Secret never expires; `kubectl -n flux-system delete secret infrastructure-verify-credentials`; `flux reconcile kustomization platform-policy` recreates it with a new token; set `kubernetes-token`; `ansible-playbook ansible/reconciler.yml` |
| Rollback | `systemctl disable --now infra-reconcile-verify.timer`; no other system depends on the host |

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
