# Requirements

| Scope                    | Tools                                              |
| ------------------------ | -------------------------------------------------- |
| Native development shell | `nix develop`; dependencies pinned by `flake.lock` |
| Host configuration       | Ansible from `uv sync --frozen --group ci`         |
| Cluster                  | kubectl, Flux, Helm and Kustomize                  |
| Provider resources       | OpenTofu, AWS CLI and provider credentials         |
| Secrets                  | SOPS, age and Doppler                              |
| Repository and releases  | Git, GitHub CLI, actionlint                        |
| Recovery                 | Restic and matching native database tools          |

```sh
nix develop
uv sync --frozen --group ci
```

[Toolchain](../flake.nix) · [Version pins](../platform/versions.yaml) · [Operation](platform.md)
