# Secrets

| Scope                                 | Location                                                                                                                    |
| ------------------------------------- | --------------------------------------------------------------------------------------------------------------------------- |
| Infrastructure APIs, runner App, mail | Doppler `infra → ops`                                                                                                       |
| Package signing and AUR keys          | Doppler `infra → ops` (`PACKAGES_GPG_KEY`, `PACKAGES_APK_KEY`, `AUR_SSH_KEY`); `fredrir/packages` environment `publish`     |
| Build cache keys                      | `platform/components/runners/<project>/sccache-*.secret.sops.yaml`, `platform/components/build-cache/**/*.secret.sops.yaml` |
| Release tokens                        | None; crates.io trusted publishing and Octo STS                                                                             |
| Application source environments       | Each project's existing Doppler configuration                                                                               |
| Cluster runtime secrets               | `platform/**/*.secret.sops.yaml`                                                                                            |
| Independent monitoring                | `ansible/roles/gatus/files/config.sops.yaml`                                                                                |
| Decryption                            | Macie, Archie and Flux age recipients                                                                                       |
| Host tokens                           | Private `/etc/rancher/k3s/server-token` and `agent-token`                                                                   |
| Recovery archives                     | Private `.infra/` on Macie and independent Archie copies                                                                    |
| Git                                   | Encrypted values only; private keys and decrypted files stay outside tracked paths                                          |

```sh
export SOPS_AGE_KEY_FILE=/path/to/age-key.txt
sops platform/projects/<project>/<name>.secret.sops.yaml
```

[Platform operation](platform.md) · [Mail credentials](mail-alerts.md)
