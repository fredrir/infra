# Local development

| Path | Contents |
| --- | --- |
| `internal/dev` | `infra dev` operations |
| `internal/cli/dev.go` | `infra dev` commands |
| `.cache/dev` | Local state; ignored by Git |
| `.cache/dev/tools` | Pinned tools from `infra dev setup` |
| `.cache/dev/render` | Offline Flux renders |
| `.cache/dev/engine` | Engine GC policy |
| `.cache/dev/bin` | Binary built for qualification suites |

```sh
infra dev doctor
infra dev setup
infra dev clean
infra dev render --project llunde
infra dev render --out - | kubectl apply --dry-run=server -f -
infra dev diff
infra dev engine start
infra dev qualify onboarding
infra dev qualify packages -- -timeout=20m
infra dev clean --all
```

| Suite | Gate | Needs |
| --- | --- | --- |
| `onboarding` | `INFRA_ONBOARD_INTEGRATION=1` | age-keygen, sops, helm, kustomize |
| `image` | `INFRA_DAGGER_IMAGE_TEST_ROOT` | engine |
| `reconcile-plan` | `INFRA_RECONCILE_PLAN_QUALIFY=1` | tofu |
| `packages` | `INFRA_PACKAGE_QUALIFY=1` | nfpm, gpg, openssl, go, engine, binary |
| `kata` | `INFRA_KATA_ENGINE_TEST=1` | kata engine profile, binary |

| Platform | Support |
| --- | --- |
| Linux amd64 | Full |
| macOS | `doctor`, `clean`, `render`, `diff`, `engine`; tools installed manually; Linux images run emulated |
