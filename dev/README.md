# Local development

| Path | Contents |
| --- | --- |
| `internal/dev` | `infra dev` operations |
| `internal/cli/dev.go` | `infra dev` commands |
| `.cache/dev` | Local state; ignored by Git |
| `.cache/dev/tools` | Pinned tools from `infra dev setup` |

```sh
infra dev doctor
infra dev setup
infra dev clean
```

| Platform | Support |
| --- | --- |
| Linux amd64 | Full |
| macOS | `doctor`, `clean`; tools installed manually |
