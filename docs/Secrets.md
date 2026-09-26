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
| Verification trigger credentials      | `ansible/roles/verification_trigger/files/credentials.sops.yaml`; Macie, Archie and `fredrir-06` |
| Control-plane backup credentials      | `ansible/roles/control_backup/files/control.sops.yaml`; Macie, Archie and `fredrir-07`; cluster maintenance copy `platform/components/backups/backup.secret.sops.yaml` |
| Decryption                            | Macie, Archie `~/.config/age/keys.txt`; Flux `flux-system/sops-age`; hosts `/etc/age/host.key`; CI apply Doppler `prd_reconciliation_apply` `SOPS_AGE_KEY` |
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
| CI apply | `age1jm6xj8qlmfjlhw0vdseaaqkpt3mqj3yl0upx3smwutsaghcq6pesvrka2t`; cluster backup secret only |

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

[Platform operation](platform.md) · [Mail credentials](mail-alerts.md)
