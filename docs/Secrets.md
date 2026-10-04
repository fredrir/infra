# Secrets

| Scope                                 | Location                                                                                                                                                                               |
| ------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Operator infrastructure APIs, runner App, mail | `secrets/operator.sops.yaml`, Macie and Archie only                                                                                                                                                           |
| CI reconciliation                    | Pull request plans: `infrastructure-plan` environment secrets; applies and verification: reconciler SOPS maps |
| Package signing and AUR keys          | `secrets/operator.sops.yaml`, Macie and Archie only (`PACKAGES_GPG_KEY`, `PACKAGES_APK_KEY`, `AUR_SSH_KEY`); `fredrir/packages` environment `publish`                                                                |
| Private image preflight | `GHCR_READ_PACKAGES_TOKEN` in `secrets/operator.sops.yaml`; dedicated classic `read:packages` token |
| Build cache keys                      | `platform/components/runners/<project>/sccache-*.secret.sops.yaml`, `platform/components/object-store/ci/<project>.secret.sops.yaml` |
| Object store                          | `platform/components/object-store/<cell>.secret.sops.yaml` (JWT, SSE KEK, TLS keys), `<cell>-identities.secret.sops.yaml` (S3 credentials), `restic.secret.sops.yaml` (primary backup repository credentials, consumer copies `platform/projects/<namespace>/backup-primary.secret.sops.yaml`), `parser-dataset-aws.secret.sops.yaml` (AWS `platform-dataset-parser` for the parser dataset copy); CA key `pki/ca.sops.yaml`, Macie and Archie only |
| Flux deploy receiver                  | `platform/components/sources/deploy-receiver.secret.sops.yaml`                                                                                                                         |
| Release tokens                        | None; crates.io trusted publishing and Octo STS                                                                                                                                        |
| Application source environments       | Each project's existing Doppler configuration                                                                                                                                          |
| Cluster runtime secrets               | `platform/**/*.secret.sops.yaml`                                                                                                                                                       |
| AWS workload access keys              | Created outside OpenTofu per `/platform/` IAM user; the consuming project's `*.secret.sops.yaml`                                                                                       |
| Independent monitoring                | Settings `ansible/roles/gatus/templates/config.yaml.j2`; secrets `ansible/roles/gatus/files/secrets.sops.yaml`; Macie, Archie and `fredrir-06` |
| Publisher App key                     | `ansible/roles/reconciler/files/credentials.sops.yaml` `["apply"]["publisher-app-key"]`; the fredrir-11 apply engine, as a consumed `0600` file, and administrators only; [publishing](runbook.md#publishing) |
| Reconciler credentials | `ansible/roles/reconciler/files/credentials.sops.yaml`, maps `verify` and `apply`; Macie, Archie and `fredrir-11`; [reconciler host](runbook.md#reconciler-host) |
| Control-plane backup credentials      | `ansible/roles/control_backup/files/control.sops.yaml`; Macie, Archie and `fredrir-07`; cluster maintenance copy `platform/components/backups/backup.secret.sops.yaml` |
| Decryption                            | Macie, Archie `~/.config/age/keys.txt`; Flux `flux-system/sops-age`; hosts `/etc/age/host.key` |
| Host tokens                           | Private `/etc/rancher/k3s/server-token` and `agent-token`                                                                                                                              |
| Host age keys                         | Root `0600` `/etc/age/host.key` on `fredrir-06`, `fredrir-07` and `fredrir-11`; generated on the host by `ansible/roles/host_secrets`, never copied; [host-scoped secrets](#host-scoped-secrets) |
| Recovery archives                     | Private `.infra/` on Macie and independent Archie copies                                                                                                                               |
| Git                                   | Encrypted values only; private keys and decrypted files stay outside tracked paths                                                                                                     |

| Recipient | Public key |
| --- | --- |
| Macie | `age1wflp6cynwm97wndq5zxmpaxwz59h62a7dku8qdyue5zm9g4djfnqwj9n0m` |
| Archie | `age1mxszcn7gs8gnvhpq8ku748szqe8u6raferefg986slu83r3zkcmswe29zs` |
| Flux | `age1eva47ddzjgmvrzjd8mxm7h0n6vvamw5xp94aqvlm2d3yf3uvracqypxu7x` |
| `fredrir-06`, `fredrir-07`, `fredrir-11` | `.sops.yaml` anchors of the same name |

| Env | Value |
| --- | --- |
| `SOPS_AGE_KEY_FILE` | `$HOME/.config/age/keys.txt`; set by `.envrc` |
| Workspace environment | `.envrc` configures tool paths, kubeconfig and the age-key file; operator values are passed only to the command that needs them |
| Rust onboarding | `onboard-rust --root` reads only the three ARC App fields from that checkout's encrypted operator file |

```sh
sops secrets/operator.sops.yaml
sops platform/projects/<project>/<name>.secret.sops.yaml
sops rotate -i --add-age "$NEW" --rm-age "$OLD" platform/projects/<project>/<name>.secret.sops.yaml
```

| Operator command | Selected fields from `secrets/operator.sops.yaml` |
| --- | --- |
| Fleet OpenTofu | `HCLOUD_TOKEN` → `TF_VAR_hcloud_token`, `CLOUDFLARE_API_TOKEN`, `PLATFORM_ALERT_RECIPIENT` → `TF_VAR_platform_mail_recipient`; [provider command](platform.md#provider-resources) |
| Reconciler OpenTofu | `HETZNER_RECONCILER_ADMIN` → `TF_VAR_reconciler_hcloud_token`, `CLOUDFLARE_OPENTOFU_ROOT` → `CLOUDFLARE_API_TOKEN` |
| Node enrollment | `TAILSCALE_ENROLL_CLIENT_ID`, `TAILSCALE_ENROLL_CLIENT_SECRET` |
| Policy administration | `TAILSCALE_POLICY_CLIENT_ID`, `TAILSCALE_POLICY_CLIENT_SECRET` |
| Package publication | `PACKAGES_GPG_KEY`, `PACKAGES_APK_KEY`; AUR additionally `AUR_SSH_KEY` |
| Private image preflight | `GHCR_READ_PACKAGES_TOKEN` |

```sh
TAILSCALE_ENROLL_CLIENT_ID="$(sops decrypt --extract '["TAILSCALE_ENROLL_CLIENT_ID"]' secrets/operator.sops.yaml)" \
TAILSCALE_ENROLL_CLIENT_SECRET="$(sops decrypt --extract '["TAILSCALE_ENROLL_CLIENT_SECRET"]' secrets/operator.sops.yaml)" \
infra operations enrollment create-deliver --node fredrir-NN --role worker
```

## Host-scoped secrets

| Unit | Host | Encrypted file | Delivery |
| --- | --- | --- | --- |
| `platform-control-backup.service` | `fredrir-07` | `ansible/roles/control_backup/files/control.sops.yaml` | Host key through `LoadCredential`; `sops exec-env` into the environment |
| `gatus.service` | `fredrir-06` | `ansible/roles/gatus/files/secrets.sops.yaml` | Root `ExecStartPre` decrypts to an `EnvironmentFile` removed after start; Gatus substitutes `${NAME}` in its settings |
| `infra-reconcile-verify.service` | `fredrir-11` | `ansible/roles/reconciler/files/credentials.sops.yaml` | Root `ExecStartPre` decrypts only the `verify` map into the unit's runtime directory; the supervisor deletes it once read |
| `infra-reconcile-apply.service` | `fredrir-11` | `ansible/roles/reconciler/files/credentials.sops.yaml` | Credential-free `ExecCondition`; then a root `ExecStartPre` decrypts only the `apply` map into the unit's runtime directory; the supervisor deletes it once read |

| Name | Value |
| --- | --- |
| Key | Root `0600` `/etc/age/host.key`, generated once by `host_secrets` |
| Recipient | `.sops.yaml` anchor named after the host |
| Host copy | Ciphertext `0600` next to the unit's settings |
| Install check | Host recipient present; host decrypts the declared ciphertext before it replaces the installed copy |
| Comparison | Ciphertext, settings and units; never plaintext |
| Lost key | The next reconcile generates a new key; enroll its recipient |

| Enroll a host | Command |
| --- | --- |
| Read recipient | `ssh root@<host> age-keygen -y /etc/age/host.key` |
| Declare | Set the host anchor in `.sops.yaml` and add it to the host's creation rule |
| Re-encrypt | `sops updatekeys -y ansible/roles/<role>/files/<file>.sops.yaml` |
| Apply | Merge; reconciliation installs the ciphertext |

```sh
sops ansible/roles/gatus/files/secrets.sops.yaml
jq -Rs 'rtrimstr("\n")' < NEW_PASSWORD | sops set --value-stdin ansible/roles/control_backup/files/control.sops.yaml '["RESTIC_PASSWORD"]'
```

| Rotate | Change together | Authority | Verify | Roll back |
| --- | --- | --- | --- | --- |
| Verification heartbeat | `ansible/roles/reconciler/files/credentials.sops.yaml` `["verify"]["gatus-token"]`; Gatus `GATUS_TOKEN_RECONCILIATION_VERIFICATION` | None | Next hourly verification reports; the previous token gets 401 | Revert |
| Backup heartbeat `<name>` | Gatus `GATUS_TOKEN_BACKUPS_<NAME>`; the producer's `BACKUP_HEARTBEAT_TOKEN`: `control.sops.yaml` and `platform/components/backups`, `platform/projects/{llunde-pyparser,y,portfolio}` | None | `kubectl -n <namespace> create job --from=cronjob/data-backup <name>` or a control backup reports success; the previous token gets 401 | Revert |
| Apply heartbeat | `ansible/roles/reconciler/files/credentials.sops.yaml` `["apply"]["gatus-token"]`; Gatus `GATUS_TOKEN_RECONCILIATION_APPLY` | None | `infra reconcile run request` and `systemctl start infra-reconcile-apply.service` on `fredrir-11` report; the previous token gets 401 | Revert |
| SMTP | Gatus `GATUS_SMTP_*`; `platform/components/observability/alertmanager.secret.sops.yaml`; `secrets/operator.sops.yaml` `PLATFORM_WATCHDOG_SMTP_*` | Administrator IAM: second access key on `fredrir-platform-alerts-smtp`, [SES SMTP derivation](https://docs.aws.amazon.com/ses/latest/dg/smtp-credentials.html) | Verify installed Gatus and mounted Alertmanager credentials, then STARTTLS AUTH from `fredrir-06`; deactivate and delete the previous key | Before deletion: reactivate the previous key; revert |
| Control backup access key | `control.sops.yaml` and `platform/components/backups/backup.secret.sops.yaml` `AWS_*` | Administrator IAM: second access key on `platform-restic-control` | Control backup and `repository-maintenance` job succeed; `aws iam get-access-key-last-used`; then deactivate and delete the previous key | Before deletion: reactivate the previous key; revert |
| Control repository password | `control.sops.yaml` and `platform/components/backups/backup.secret.sops.yaml` `RESTIC_PASSWORD` | Repository access with the current password | `restic key add`; control backup and `repository-maintenance` job succeed with the new password; then `restic key remove` the previous key | Revert until the previous key is removed |
| Primary repository key `<project>` | `platform/components/object-store/restic.secret.sops.yaml` `RESTIC_<PROJECT>_*` and `platform/projects/<namespace>/backup-primary.secret.sops.yaml` `AWS_*` | None | Merge; `kubectl -n object-store rollout restart statefulset/seaweedfs-hel1`; `kubectl -n <namespace> create job --from=cronjob/data-backup <name>` succeeds | Revert, then restart the cell |
| Primary repository password `<project>` | `platform/projects/<namespace>/backup-primary.secret.sops.yaml` `RESTIC_PASSWORD` | Repository access with the current password | `restic key add`; `data-backup` and `repository-maintenance` jobs succeed with the new password; then `restic key remove` the previous key | Revert until the previous key is removed |

`restic key add` and `restic key remove` replace the password that unlocks the repository master key, not the master key; replacing that requires a new repository and is only needed after a suspected compromise.

## Provenance token

| Name | Value |
| --- | --- |
| Kind | Personal access token (classic), scope `read:packages` only; GitHub Packages accepts neither fine-grained tokens nor App installation tokens outside Actions |
| Use | `PROVENANCE_TOKEN` of the fredrir-11 gate and engine: public pull request reads, `gh attestation verify`, `cosign verify` of private GHCR deployment images |
| Location | `ansible/roles/reconciler/files/credentials.sops.yaml` `["apply"]["provenance-token"]` |
| Lifetime | No expiry |
| Alert | Gatus `reconciliation_apply` fails daily from 30 days before an expiring token's expiry |
| Rotate | Generate a replacement with the same scope; `sops set`; `(cd ansible && ansible-playbook reconciler.yml --tags host_key,reconciler_credentials)`; confirm the next readiness check or apply; revoke the previous token |

## Credential rotation

Keep the previous credential active until every consumer authenticates with the replacement; then revoke it at its issuer and confirm it is rejected. Reconciler credentials require `(cd ansible && ansible-playbook reconciler.yml --tags host_key,reconciler_credentials)`; ordinary reconciliation does not install that role.

| Credential | Consumers | Replacement and verification |
| --- | --- | --- |
| Runner App key | Reconciler `apply.runner-app-key`; operator `ARC_GITHUB_APP_PRIVATE_KEY`; ARC `github-app.secret.sops.yaml` in `platform/components/runners/{infra,nsql,packages}` | Generate an App key, update every copy and install reconciler credentials; refresh ARC clients and verify a fresh runner registration and job before deleting the previous key |
| Publisher App key | Reconciler `apply.publisher-app-key` | Generate an App key and install reconciler credentials; verify successful production publication before deleting the previous key |
| Observer App key | Reconciler `verify.observer-app-key` | Generate an App key and install reconciler credentials; verify ruleset and runner reads in a full verification before deleting the previous key |
| Fleet Hetzner write token | Reconciler `apply.hcloud-token`; operator `HCLOUD_TOKEN` | Fleet project → Security → API tokens → Read & Write; update both copies, install reconciler credentials and verify a full apply |
| Fleet Hetzner verification token | Reconciler `verify.hcloud-token`; operator `HETZNER_RECONCILIATION_VERIFY` | Fleet project → Security → API tokens → Read; update both copies, install reconciler credentials and verify full verification |
| Reconciler Hetzner administrator token | Operator `HETZNER_RECONCILER_ADMIN` | Reconciler project → Security → API tokens → Read & Write; verify the reconciler OpenTofu plan |
| Plan credentials | GitHub `infrastructure-plan`: AWS pair, `CLOUDFLARE_API_TOKEN`, `HCLOUD_TOKEN`, `KUBE_CONFIG` | Second key on `/automation/infra-reconciliation-plan`, scoped Cloudflare/Hetzner read tokens and replacement Kubernetes token; update each secret and verify a hosted `infra reconcile plan --full` |
| Kubernetes service-account tokens | `flux-system/infrastructure-plan` and `infrastructure-apply`; GitHub plan kubeconfig and reconciler `apply.kubernetes-token` | Declare a new token Secret alongside the current Secret; activate consumers and verify a full provider plan and full apply; separately authenticate a live Flux read and confirm deployment creation is forbidden with the new plan token before removing the previous Secret |
| Operator Cloudflare token | Operator `CLOUDFLARE_API_TOKEN` | Managed-zone Zone Read/DNS Write and account Tunnel Write; verify a fleet OpenTofu plan |
| Cloudflare token administrator | Operator `CLOUDFLARE_OPENTOFU_ROOT` | Account API Tokens Edit for the infrastructure account; verify account-token inventory and the reconciler OpenTofu plan |
| Linode token | Operator `LINODE_TOKEN` | Cloud Manager → Profile → API Tokens; retain `linodes:read_write`, verify the managed instance and any external consumers |
| Enrollment OAuth | Operator `TAILSCALE_ENROLL_CLIENT_ID`, `TAILSCALE_ENROLL_CLIENT_SECRET` | `auth_keys`, owner tag `tag:platform-enrollment`; replace ID and secret together, verify control, worker and volatile enrollment |
| Policy OAuth | Operator `TAILSCALE_POLICY_CLIENT_ID`, `TAILSCALE_POLICY_CLIENT_SECRET` | `policy_file`, `devices:core:read`, `devices:posture_attributes`; replace ID and secret together, verify policy validation and device inventory |
| Provenance PAT | Reconciler `apply.provenance-token`; operator `GHCR_PROVENANCE_PAT` | Update both copies and install reconciler credentials; verify private image provenance; [token requirements](#provenance-token) |
| Private image reader | Operator `GHCR_READ_PACKAGES_TOKEN` | Dedicated classic `read:packages` token; verify authenticated private image preflight |
| Package signing and AUR keys | Operator `PACKAGES_GPG_KEY`, `PACKAGES_APK_KEY`, `AUR_SSH_KEY`; `fredrir/packages` environment `publish` | Distribute replacement public signing keys before publication; verify package signatures and AUR publishing before retiring previous keys |

```sh
jq -Rs 'rtrimstr("\n")' < NEW.pem | sops set --value-stdin ansible/roles/reconciler/files/credentials.sops.yaml '["apply"]["runner-app-key"]'
(cd ansible && ansible-playbook reconciler.yml --tags host_key,reconciler_credentials)
gh secret set HCLOUD_TOKEN --env infrastructure-plan < NEW_TOKEN
```

Reconciler provisioning uses a fresh one-use, preauthorized, non-ephemeral auth key with `tag:infra-reconciler`; an enrolled node uses its own node identity.

## Retire a recipient

| Step | Action |
| --- | --- |
| Re-encrypt | Remove the anchor from `.sops.yaml`; `sops rotate -i --rm-age <recipient> <file>` for each file `TestSOPSFilesUseOnlyDeclaredRecipients` names |
| Delete the key | Remove the private key from every store that holds it |
| Rotate | Every value any Git revision encrypted to the recipient |

[Platform operation](platform.md) · [Mail credentials](mail-alerts.md)
