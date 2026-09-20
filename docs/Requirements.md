# Requirements

| Scope                    | Tools                                              |
| ------------------------ | -------------------------------------------------- |
| Repository development | Go and Bazel; pins in `build/toolchain.json` |
| Container builds | Dagger engine; compiled Go SDK in `infra` |
| Binary consumers | Verified `infra` artifact; no Go compiler required |
| Host configuration       | Ansible from `uv sync --frozen --group ci`         |
| Cluster                  | kubectl, Flux, Helm and Kustomize                  |
| Provider resources       | OpenTofu, AWS CLI and provider credentials         |
| Secrets                  | SOPS, age and Doppler                              |
| Repository and releases  | Git, GitHub CLI, actionlint                        |
| Recovery                 | Restic and matching native database tools          |

```sh
go test ./...
bazel build //cmd/infra:infra
uv sync --frozen --group ci
```

[Development](development.md) · [Toolchain](../build/toolchain.json) · [Version pins](../platform/versions.yaml) · [Operation](platform.md)
