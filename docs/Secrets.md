# Secrets

| Scope                                 | Location                                                                                                                                                                               |
| ------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Operator infrastructure APIs, runner App, mail | Doppler `infra → ops`                                                                                                                                                           |
| CI reconciliation                    | Pull request plans: `infrastructure-plan` environment secrets; hosted verification: Doppler `infra → prd_reconciliation_apply` through `infrastructure-apply`'s config-scoped `DOPPLER_TOKEN` |
| Package signing and AUR keys          | Doppler `infra → ops` (`PACKAGES_GPG_KEY`, `PACKAGES_APK_KEY`, `AUR_SSH_KEY`); `fredrir/packages` environment `publish`                                                                |
| Build cache keys                      | `platform/components/runners/<project>/sccache-*.secret.sops.yaml`, `platform/components/object-store/ci/<project>.secret.sops.yaml` |
| Object store                          | `platform/components/object-store/<cell>.secret.sops.yaml` (JWT, SSE KEK, TLS keys), `<cell>-identities.secret.sops.yaml` (S3 credentials); CA key `pki/ca.sops.yaml`, Macie and Archie only |
| Flux deploy receiver                  | `platform/components/sources/deploy-receiver.secret.sops.yaml`                                                                                                                         |
| Release tokens                        | None; crates.io trusted publishing and Octo STS                                                                                                                                        |
| Application source environments       | Each project's existing Doppler configuration                                                                                                                                          |
| Cluster runtime secrets               | `platform/**/*.secret.sops.yaml`                                                                                                                                                       |
| AWS workload access keys              | Created outside OpenTofu per `/platform/` IAM user; the consuming project's `*.secret.sops.yaml`                                                                                       |
| Independent monitoring                | Settings `ansible/roles/gatus/templates/config.yaml.j2`; secrets `ansible/roles/gatus/files/secrets.sops.yaml`; Macie, Archie and `fredrir-06` |
| Publisher App key                     | `ansible/roles/reconciler/files/credentials.sops.yaml` `["apply"]["publisher-app-key"]`; the fredrir-11 apply engine, as a consumed `0600` file, and administrators only; [publishing](runbook.md#publishing) |
| Reconciler credentials | `ansible/roles/reconciler/files/credentials.sops.yaml`, maps `verify` and `apply`; Macie, Archie and `fredrir-11`; [reconciler host](runbook.md#reconciler-host) |
| Verification trigger credentials      | `ansible/roles/verification_trigger/files/credentials.sops.yaml`; Macie, Archie and `fredrir-06` |
| Control-plane backup credentials      | `ansible/roles/control_backup/files/control.sops.yaml`; Macie, Archie and `fredrir-07`; cluster maintenance copy `platform/components/backups/backup.secret.sops.yaml` |
| Decryption                            | Macie, Archie `~/.config/age/keys.txt`; Flux `flux-system/sops-age`; hosts `/etc/age/host.key` |
| Host tokens                           | Private `/etc/rancher/k3s/server-token` and `agent-token`                                                                                                                              |
| Host age keys                         | Root `0600` `/etc/age/host.key` on `fredrir-06` and `fredrir-07`; generated on the host by `ansible/roles/host_secrets`, never copied; [host-scoped secrets](#host-scoped-secrets) |
| Recovery archives                     | Private `.infra/` on Macie and independent Archie copies                                                                                                                               |
| Git                                   | Encrypted values only; private keys and decrypted files stay outside tracked paths                                                                                                     |

| Recipient | Public key |
| --- | --- |
| Macie | `age1wflp6cynwm97wndq5zxmpaxwz59h62a7dku8qdyue5zm9g4djfnqwj9n0m` |
| Archie | `age1mxszcn7gs8gnvhpq8ku748szqe8u6raferefg986slu83r3zkcmswe29zs` |
| Flux | `age1eva47ddzjgmvrzjd8mxm7h0n6vvamw5xp94aqvlm2d3yf3uvracqypxu7x` |
| `fredrir-06`, `fredrir-07` | `.sops.yaml` anchors of the same name |

| Env | Value |
| --- | --- |
| `SOPS_AGE_KEY_FILE` | `$HOME/.config/age/keys.txt`; set by `.envrc` |

```sh
sops platform/projects/<project>/<name>.secret.sops.yaml
sops rotate -i --add-age "$NEW" --rm-age "$OLD" platform/projects/<project>/<name>.secret.sops.yaml
```

## Host-scoped secrets

| Unit | Host | Encrypted file | Delivery |
| --- | --- | --- | --- |
| `platform-control-backup.service` | `fredrir-07` | `ansible/roles/control_backup/files/control.sops.yaml` | Host key through `LoadCredential`; `sops exec-env` into the environment |
| `gatus.service` | `fredrir-06` | `ansible/roles/gatus/files/secrets.sops.yaml` | Root `ExecStartPre` decrypts to an `EnvironmentFile` removed after start; Gatus substitutes `${NAME}` in its settings |
| `infra-verification-request.service` | `fredrir-06` | `ansible/roles/verification_trigger/files/credentials.sops.yaml` | Root `ExecStartPre` decrypts into the unit's runtime directory |
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
| Verification heartbeat | `credentials.sops.yaml` `token`; Gatus `GATUS_TOKEN_RECONCILIATION_VERIFICATION` | None | Next hourly verification reports; the previous token gets 401 | Revert |
| Backup heartbeat `<name>` | Gatus `GATUS_TOKEN_BACKUPS_<NAME>`; the producer's `BACKUP_HEARTBEAT_TOKEN`: `control.sops.yaml` and `platform/components/backups`, `platform/projects/{llunde-pyparser,y,portfolio}` or `platform/components/cache` for `attic` | None | `kubectl -n <namespace> create job --from=cronjob/data-backup <name>` or a control backup reports success; the previous token gets 401 | Revert |
| Verification App key | `credentials.sops.yaml` `private_key` | GitHub App settings | Next verification dispatches; then delete the previous key in the App | Revert while the previous key exists |
| Apply heartbeat | `ansible/roles/reconciler/files/credentials.sops.yaml` `["apply"]["gatus-token"]`; Gatus `GATUS_TOKEN_RECONCILIATION_APPLY` | None | `infra reconcile run request` and `systemctl start infra-reconcile-apply.service` on `fredrir-11` report; the previous token gets 401 | Revert |
| SMTP | Gatus `GATUS_SMTP_*`; `platform/components/observability/alertmanager.secret.sops.yaml`; Doppler `infra/ops` `PLATFORM_WATCHDOG_SMTP_*` | Administrator IAM: second access key on `fredrir-platform-alerts-smtp`, [SES SMTP derivation](https://docs.aws.amazon.com/ses/latest/dg/smtp-credentials.html) | SMTP login from `fredrir-06`; Alertmanager delivers; then deactivate and delete the previous key | Reactivate the previous key; revert |
| Control backup access key | `control.sops.yaml` and `platform/components/backups/backup.secret.sops.yaml` `AWS_*` | Administrator IAM: second access key on `platform-restic-control` | Control backup and `repository-maintenance` job succeed; `aws iam get-access-key-last-used`; then deactivate and delete the previous key | Reactivate the previous key; revert |
| Control repository password | `control.sops.yaml` and `platform/components/backups/backup.secret.sops.yaml` `RESTIC_PASSWORD` | Repository access with the current password | `restic key add`; control backup and `repository-maintenance` job succeed with the new password; then `restic key remove` the previous key | Revert until the previous key is removed |

`restic key add` and `restic key remove` replace the password that unlocks the repository master key, not the master key; replacing that requires a new repository and is only needed after a suspected compromise.

## Provenance token

| Name | Value |
| --- | --- |
| Kind | Personal access token (classic), scope `read:packages` only; GitHub Packages accepts neither fine-grained tokens nor App installation tokens outside Actions |
| Use | `PROVENANCE_TOKEN` of the fredrir-11 gate and engine: public pull request reads, `gh attestation verify`, `cosign verify` of private GHCR deployment images |
| Location | `ansible/roles/reconciler/files/credentials.sops.yaml` `["apply"]["provenance-token"]` |
| Lifetime | No expiry |
| Alert | Gatus `reconciliation_apply` fails daily from 30 days before an expiring token's expiry |
| Rotate | Generate a replacement with the same scope; `sops set`; `ansible-playbook ansible/reconciler.yml`; confirm the next readiness check or apply; revoke the previous token |

## Credentials shared with Doppler

These values were copied from Doppler, which still holds them; rotate each before its Doppler copy is deleted.

| Credential | Copies | Rotate | Verify, then retire the previous value |
| --- | --- | --- | --- |
| Runner App key | `["apply"]["runner-app-key"]`; Doppler `prd_reconciliation_apply` `RUNNER_APP_PRIVATE_KEY` | Generate a key in the runner App; `credential runner-app-key < NEW.pem`; `ansible-playbook ansible/reconciler.yml` | A tooling or Ansible apply mints the runner token; delete the previous key in the App |
| Publisher App key | `["apply"]["publisher-app-key"]`; Doppler `prd_reconciliation_apply` `PUBLISHER_APP_PRIVATE_KEY` | Generate a key in the publisher App; `credential publisher-app-key < NEW.pem`; `ansible-playbook ansible/reconciler.yml` | An apply publishes `production`; delete the previous key in the App |
| Observer App key | `["verify"]["observer-app-key"]`; Doppler `prd_reconciliation_apply` `OBSERVER_APP_PRIVATE_KEY` | Generate a key in the observer App; set it in the `verify` map; `ansible-playbook ansible/reconciler.yml` | The next verification reads rulesets and runners; delete the previous key in the App |
| Fleet Hetzner read/write token | `["apply"]["hcloud-token"]`; Doppler `prd_reconciliation_apply` `HCLOUD_TOKEN` | Hetzner console, fleet project, Security, API tokens: generate Read & Write; `credential hcloud-token`; `ansible-playbook ansible/reconciler.yml` | `infra reconcile run request --full` applies; delete the previous token |
| Fleet Hetzner read token | `infrastructure-plan` `HCLOUD_TOKEN`; Doppler `prd_reconciliation_plan` `HCLOUD_TOKEN` | Generate Read; `gh secret set HCLOUD_TOKEN --env infrastructure-plan` | The next pull request plan passes; delete the previous token |
| Plan AWS key, Cloudflare token, kubeconfig | `infrastructure-plan`; Doppler `prd_reconciliation_plan` | Second access key on `/automation/infra-reconciliation-plan`; new read token for managed zones and tunnels; `flux-system/infrastructure-plan` token; `gh secret set` each | The next pull request plan passes; delete the previous key and token |

`credential NAME` is `jq -Rs 'rtrimstr("\n")' | sops set --value-stdin ansible/roles/reconciler/files/credentials.sops.yaml "[\"apply\"][\"NAME\"]"`.

## Retire a recipient

| Step | Action |
| --- | --- |
| Re-encrypt | Remove the anchor from `.sops.yaml`; `sops rotate -i --rm-age <recipient> <file>` for each file `TestSOPSFilesUseOnlyDeclaredRecipients` names |
| Delete the key | Remove the private key from every store that holds it |
| Rotate | Every value any Git revision encrypted to the recipient |

[Platform operation](platform.md) · [Mail credentials](mail-alerts.md)
