# Local development

| Path | Contents |
| --- | --- |
| `internal/dev` | `infra dev` operations |
| `internal/cli/dev.go` | `infra dev` commands |
| `.cache/dev` | Local state; ignored by Git |
| `.cache/dev/tools` | Pinned tools from `infra dev setup` |
| `.cache/dev/render` | Offline Flux renders |

```sh
infra dev doctor
infra dev setup
infra dev clean
infra dev render --project llunde
infra dev render --out - | kubectl apply --dry-run=server -f -
infra dev diff
```

| Platform | Support |
| --- | --- |
| Linux amd64 | Full |
| macOS | `doctor`, `clean`, `render`, `diff`; tools installed manually |
