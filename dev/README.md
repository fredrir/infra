# Local development

| Path | Contents |
| --- | --- |
| `internal/dev` | `infra dev` operations |
| `internal/cli/dev.go` | `infra dev` commands |
| `.cache/dev` | Local state; ignored by Git |
| `.cache/dev/tools` | Pinned tools from `infra dev setup` |
| `.cache/dev/render` | Offline Flux renders |
| `.cache/dev/engine` | Engine GC policy |
| `.cache/dev/bin` | `infra` built by `setup`, qualification suites and benchmark scenarios; first on `PATH` through `.envrc` |
| `dev/cluster/k3d.yaml` | Cluster shape, pinned K3s image, local registry |
| `dev/cluster/patches.yaml` | Flux Kustomization patches applied in the dev cluster |
| `dev/cluster/secrets/` | Plaintext Secrets replacing synthesized placeholders, same relative path as under `platform/` |
| `.cache/dev/cluster` | Kubeconfig, dev age key, pushed artifact, generated `root.yaml` |
| `dev/hosts/hosts.yaml` | Guest nodes, pinned Ubuntu cloud image, multicast segment |
| `.cache/dev/hosts` | Base image, guest disks, seeds, serial logs, SSH key, inventory, known hosts |
| `.cache/dev/reconciler` | SeaweedFS and Gatus binaries and per-run scratch of the `reconciler` suite |
| `dev/bench/scenarios.yaml` | Commands, optional setup, runs, warmups and budgets sampled by hyperfine; `{infra}` is the built binary |
| `.cache/dev/bench` | Timestamped samples, `latest.json`, `go-baseline.txt` |

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
infra dev cluster up
infra dev cluster sync --profile platform
KUBECONFIG=.cache/dev/cluster/kubeconfig kubectl get kustomizations -A
infra dev cluster down
infra dev hosts up
infra dev hosts up dev-reconciler-1
infra dev qualify reconciler -- -timeout=60m
infra dev hosts check site.yml
infra dev hosts play site.yml
infra dev hosts ssh dev-server-1 -- sudo k3s kubectl get nodes
infra dev hosts down --purge
infra dev bench run
cp .cache/dev/bench/latest.json .cache/dev/bench/baseline.json
infra dev bench run --baseline .cache/dev/bench/baseline.json render affected
infra dev bench go ./internal/ci
infra dev clean --all
```

| Role | Guests |
| --- | --- |
| `ubuntu`, `firewall`, `k3s` | `site.yml` runs against `dev-server-1` and `dev-agent-1` |
| `reconciler` | `reconciler.yml` runs against `dev-reconciler-1`, in `reconcilers` outside `ubuntu`, with `--skip-tags=transport,infra_binary`; `hosts up` starts it only when named |
| `tailscale` | Skipped; `tailscale0` is a renamed multicast NIC carrying the fake tailnet address at MTU 1280, and a stub `tailscaled.service` satisfies the K3s unit dependency |
| `build_vm`, `build_engine`, `build_runner`, `ci_runtime`, `gatus`, `control_backup` | Need nested KVM, GitHub credentials or secrets encrypted to the guest host key |

| Cluster profile | Kustomizations |
| --- | --- |
| `minimal` | `platform-policy`, `platform-projects` |
| `platform` | minimal plus `platform-sources`, `platform-ingress`, `platform-observability`, `platform-cache`, `platform-build-cache`, `platform-backups` |
| never | `platform-controllers`, `platform-runners`, `llunde-pyparser-migration`, `llunde-pyparser-application` |

| Suite | Gate | Needs |
| --- | --- | --- |
| `onboarding` | `INFRA_ONBOARD_INTEGRATION=1` | age-keygen, sops, helm, kustomize |
| `image` | `INFRA_DAGGER_IMAGE_TEST_ROOT` | engine |
| `reconcile-plan` | `INFRA_RECONCILE_PLAN_QUALIFY=1` | tofu |
| `publishing` | `INFRA_PUBLISHING_QUALIFY=1` | live `fredrir/infra`; `GH_TOKEN` administrator with `workflow` scope; `PUBLISHER_APP_PRIVATE_KEY_FILE`; [publishing](../docs/runbook.md#publishing) |
| `kustomize` | `INFRA_KUSTOMIZE_QUALIFY=1` | kubectl |
| `packages` | `INFRA_PACKAGE_QUALIFY=1` | nfpm, gpg, openssl, go, engine, binary |
| `kata` | `INFRA_KATA_ENGINE_TEST=1` | kata engine profile, binary |
| `reconciler` | `INFRA_RECONCILER_QUALIFY=1` | `dev-reconciler-1`, started by the suite; binary; Git daemon; Go; SeaweedFS from its pinned release archive with versioning and SSE-S3, and Gatus from the role's pinned layer, both on loopback |

| Platform | Support |
| --- | --- |
| Linux amd64 | Full |
| macOS | `doctor`, `clean`, `render`, `diff`, `engine`, `cluster`, `bench`; tools installed manually; Linux images run emulated; no `hosts` |
