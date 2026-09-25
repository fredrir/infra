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
| Independent monitoring                | `ansible/roles/gatus/files/config.sops.yaml`                                                                                                                                           |
| Verification trigger App key          | `ansible/roles/verification_trigger/files/github-app.sops.yaml`                                                                                                                        |
| Decryption                            | Macie, Archie `~/.config/age/keys.txt`; Flux `flux-system/sops-age`; CI apply Doppler `prd_reconciliation_apply` `SOPS_AGE_KEY`                                                        |
| Host tokens                           | Private `/etc/rancher/k3s/server-token` and `agent-token`                                                                                                                              |
| Recovery archives                     | Private `.infra/` on Macie and independent Archie copies                                                                                                                               |
| Git                                   | Encrypted values only; private keys and decrypted files stay outside tracked paths                                                                                                     |

| Recipient | Public key |
| --- | --- |
| Macie | `age1wflp6cynwm97wndq5zxmpaxwz59h62a7dku8qdyue5zm9g4djfnqwj9n0m` |
| Archie | `age1mxszcn7gs8gnvhpq8ku748szqe8u6raferefg986slu83r3zkcmswe29zs` |
| Flux | `age1eva47ddzjgmvrzjd8mxm7h0n6vvamw5xp94aqvlm2d3yf3uvracqypxu7x` |
| CI apply | `age1jm6xj8qlmfjlhw0vdseaaqkpt3mqj3yl0upx3smwutsaghcq6pesvrka2t`; backup secret, Gatus config and verification trigger App key only |

| Env | Value |
| --- | --- |
| `SOPS_AGE_KEY_FILE` | `$HOME/.config/age/keys.txt`; set by `.envrc` |

```sh
sops platform/projects/<project>/<name>.secret.sops.yaml
sops rotate -i --add-age "$NEW" --rm-age "$OLD" platform/projects/<project>/<name>.secret.sops.yaml
```

[Platform operation](platform.md) · [Mail credentials](mail-alerts.md)
