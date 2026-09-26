# Secrets

| Scope                                 | Location                                                                                                                                                                               |
| ------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Operator infrastructure APIs, runner App, mail | Doppler `infra → ops`                                                                                                                                                           |
| CI reconciliation                    | Doppler `infra → prd_reconciliation_plan` and `infra → prd_reconciliation_apply`; each GitHub environment stores only its config-scoped `DOPPLER_TOKEN`                                  |
| Package signing and AUR keys          | Doppler `infra → ops` (`PACKAGES_GPG_KEY`, `PACKAGES_APK_KEY`, `AUR_SSH_KEY`); `fredrir/packages` environment `publish`                                                                |
| Build cache keys                      | `platform/components/runners/<project>/sccache-*.secret.sops.yaml`, `platform/components/build-cache/**/*.secret.sops.yaml`; legacy BuildKit credentials remain through caller cutover |
| Flux deploy receiver                  | `platform/components/sources/deploy-receiver.secret.sops.yaml`                                                                                                                         |
| Release tokens                        | None; crates.io trusted publishing and Octo STS                                                                                                                                        |
| Application source environments       | Each project's existing Doppler configuration                                                                                                                                          |
| Cluster runtime secrets               | `platform/**/*.secret.sops.yaml`                                                                                                                                                       |
| AWS workload access keys              | Created outside OpenTofu per `/platform/` IAM user; the consuming project's `*.secret.sops.yaml`                                                                                       |
| Independent monitoring                | Settings `ansible/roles/gatus/templates/config.yaml.j2`; secrets `ansible/roles/gatus/files/secrets.sops.yaml`; Macie, Archie and `fredrir-06` |
| Publisher App key                     | Doppler `infra → prd_reconciliation_apply` `PUBLISHER_APP_PRIVATE_KEY`; apply engine, as a consumed `0600` file, and administrators only; [publishing](runbook.md#publishing) |
| Reconciler credentials | `ansible/roles/reconciler/files/credentials.sops.yaml`; Macie, Archie and `fredrir-11`; [reconciler host](runbook.md#reconciler-host) |
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
| SMTP | Gatus `GATUS_SMTP_*`; `platform/components/observability/alertmanager.secret.sops.yaml`; Doppler `infra/ops` `PLATFORM_WATCHDOG_SMTP_*` | Administrator IAM: second access key on `fredrir-platform-alerts-smtp`, [SES SMTP derivation](https://docs.aws.amazon.com/ses/latest/dg/smtp-credentials.html) | SMTP login from `fredrir-06`; Alertmanager delivers; then deactivate and delete the previous key | Reactivate the previous key; revert |
| Control backup access key | `control.sops.yaml` and `platform/components/backups/backup.secret.sops.yaml` `AWS_*` | Administrator IAM: second access key on `platform-restic-control` | Control backup and `repository-maintenance` job succeed; `aws iam get-access-key-last-used`; then deactivate and delete the previous key | Reactivate the previous key; revert |
| Control repository password | `control.sops.yaml` and `platform/components/backups/backup.secret.sops.yaml` `RESTIC_PASSWORD` | Repository access with the current password | `restic key add`; control backup and `repository-maintenance` job succeed with the new password; then `restic key remove` the previous key | Revert until the previous key is removed |

`restic key add` and `restic key remove` replace the password that unlocks the repository master key, not the master key; replacing that requires a new repository and is only needed after a suspected compromise.

## Retire a recipient

| Step | Action |
| --- | --- |
| Re-encrypt | Remove the anchor from `.sops.yaml`; `sops rotate -i --rm-age <recipient> <file>` for each file `TestSOPSFilesUseOnlyDeclaredRecipients` names |
| Delete the key | Remove the private key from every store that holds it |
| Rotate | Every value any Git revision encrypted to the recipient |

[Platform operation](platform.md) · [Mail credentials](mail-alerts.md)
