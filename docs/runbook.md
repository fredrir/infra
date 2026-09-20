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
| Scheduled recovery | `infra reconcile apply --full` | All systems reconciled at minutes 17 and 47 every hour |
| Verification | `infra reconcile verify` | Exact Flux revision, observed generations, Helm readiness, host checks, Grafana configuration and HTTP health, frontend revision |
| Status | `infra reconcile status` | Desired revision, successfully applied revision, failing stage and stage durations |

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
| Run deadline / abandoned lock expiry | 90 minutes / 2 hours |
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
| 3 | Check out that exact `main` revision, build the CLI and run `infra reconcile apply --full` with the administrator session |
| 4 | Confirm the run created the IAM users and Flux service accounts, published `production`, switched the Flux source and completed verification |
| 5 | Create GitHub environments `infrastructure-plan` and `infrastructure-apply`; restrict `infrastructure-apply` to `main` without deployment reviewers |
| 6 | Configure the environment secrets below; use separate plan and apply identities |
| 7 | Install the dedicated apply SSH public key on inventory hosts and the build guest; retain existing administrator keys |
| 8 | Apply `tailscale/policy.hujson` to authorize the new CI tags; issue scoped, ephemeral-device enrollment keys |
| 9 | Run the workflow manually and confirm `desired_revision == applied_revision` with `stage == complete` |

The first merge requires the administrator bootstrap before CI can deploy; Flux may report the missing `production` branch until that run publishes it.

| Environment secret | `infrastructure-plan` | `infrastructure-apply` |
| --- | --- | --- |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | Keys for `/automation/infra-reconciliation-plan` | Keys for `/automation/infra-reconciliation-apply` |
| `CLOUDFLARE_API_TOKEN` | Read managed DNS zones and account tunnels | Edit managed DNS zones and account tunnels |
| `HCLOUD_TOKEN` | Read managed Hetzner project | Read/write managed Hetzner project |
| `KUBE_CONFIG` | `flux-system/infrastructure-plan` identity | `flux-system/infrastructure-apply` identity |
| `TAILSCALE_AUTH_KEY` | Ephemeral devices tagged `tag:infra-plan` | Ephemeral devices tagged `tag:infra-apply` |
| `SSH_PRIVATE_KEY` | Unset | Dedicated key for managed hosts and build guest |
| `SSH_KNOWN_HOSTS` | Unset | Verified Tailnet host keys, `fredrir-06` and `infra-build-09` aliases |
| `SOPS_AGE_KEY` | Unset | Decrypt host monitoring and backup credentials |
| `RUNNER_REGISTRATION_TOKEN` | Unset | Register missing runners for inventory repositories |

| Credential boundary | Value |
| --- | --- |
| AWS identity policies | `tofu/reconciliation.tf`; deployment identities cannot change their own grants |
| Kubernetes identities | `platform/components/policy/reconciliation.yaml` |
| Kubernetes tokens | Controller-populated `infrastructure-plan-credentials` and `infrastructure-apply-credentials` Secrets in `flux-system` |
| Kubernetes API address | Reachable control-plane Tailnet address with a matching certificate; include its CA in each kubeconfig |
| Kubernetes apply scope | Read Flux resources; patch reconciliation annotations; admission rejects spec changes |
| Tailnet plan scope | Control-plane API only |
| Tailnet apply scope | Control-plane API and SSH to managed hosts |
| Provider-policy changes or revoked credentials | Administrator repair required |

```sh
gh secret set AWS_ACCESS_KEY_ID --env infrastructure-plan < /private/plan-access-key-id
gh secret set AWS_SECRET_ACCESS_KEY --env infrastructure-plan < /private/plan-secret-access-key
gh secret set KUBE_CONFIG --env infrastructure-plan < /private/plan-kubeconfig
gh secret set AWS_ACCESS_KEY_ID --env infrastructure-apply < /private/apply-access-key-id
gh secret set AWS_SECRET_ACCESS_KEY --env infrastructure-apply < /private/apply-secret-access-key
gh secret set KUBE_CONFIG --env infrastructure-apply < /private/apply-kubeconfig
gh workflow run reconcile.yml --ref main
```

### Recovery

| Failure | Recovery |
| --- | --- |
| Missing credentials or private connectivity | Repair the environment identity, host enrollment or Tailnet policy; rerun the workflow |
| Newer merge supersedes a queued run | Reconcile current `main`; stale runs cannot publish or report success |
| Failed apply or verification | Rerun the workflow or run `infra reconcile apply --full` from a clean current `main` checkout |
| Process terminated without lock cleanup | Wait for the recorded lock expiry; scheduled reconciliation retries automatically |
| Remaining OpenTofu drift | Inspect the final plan; nonzero drift keeps the run failed |
| Image publication succeeds | Check the separate reconciliation workflow for production readiness |

Do not remove an active reconciliation or OpenTofu lock while its writer is running.
