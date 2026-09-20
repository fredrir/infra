# Development

## Architecture

| Layer | Owner | Contract |
| --- | --- | --- |
| Commands | `cmd/infra`, `internal/cli` | Cobra flags, environment conversion, cancellation, output |
| Infrastructure operations | `internal/ci`, `internal/platformops` | Typed inputs; injectable process and HTTP boundaries |
| Runtime artifacts | `internal/kata` | Pinned sources; verified downloads; native qualification |
| Builds | Bazel | Explicit dependency graph; pinned Go SDK; reusable actions |
| Container execution | Dagger Go SDK | Compiled into `infra`; pinned engine; separate execution cache |
| Binary installation | `internal/artifact` | Exact revision, platform and trusted SHA-256; atomic installation |
| Process lifecycle | `internal/process` | Context cancellation; process-group termination; bounded capture |
| Object storage | `internal/objectstore` | S3 request signing; bounded artifact transfer |
| Resource ownership | OpenTofu, Ansible, Flux | Provider resources, host configuration, cluster reconciliation |

## Local commands

```sh
go test ./...
go test -race ./...
bazel test //...
bazel build //cmd/infra:infra
bazel run //:gazelle
git diff --exit-code -- '*BUILD.bazel'
./bazel-bin/cmd/infra/infra_/infra doctor
./bazel-bin/cmd/infra/infra_/infra pipeline check --local
./bazel-bin/cmd/infra/infra_/infra pipeline check --report-dir .cache/reports
```

| Setting | Source |
| --- | --- |
| Go dependencies | `go.mod`, `go.sum` |
| Bazel dependencies | `MODULE.bazel`, `MODULE.bazel.lock` |
| Bazel executable | `.bazelversion`, `build/toolchain.json` |
| Dagger engine and build image | `build/toolchain.json` |
| Command discovery | `infra --help`; `infra <command> --help` |
| Human progress | Standard error |
| Structured results | Standard output; command-specific JSON |
| Build evidence | Result JSON, Bazel build events and timing profile |

## Binary reuse

| Consumer | Binary identity | Cache |
| --- | --- | --- |
| Reusable GitHub workflow | `job.workflow_sha`, Linux amd64, pinned toolchain | Exact-key binary cache; verified workflow artifact |
| Workstation or host | Full revision, OS/architecture, trusted SHA-256 | Private user cache; checksum revalidated on every install |
| Container image | Verified precompiled Linux binary | Image layers; digest-pinned deployment |

```sh
infra artifact install \
  --url "$INFRA_BINARY_URL" \
  --revision "$INFRA_REVISION" \
  --sha256 "$INFRA_SHA256" \
  --platform linux/amd64 \
  --destination "$HOME/.local/bin/infra"
infra artifact prune --max-bytes 1073741824 --max-age 720h --keep-revision "$INFRA_REVISION"
```

The SHA-256 and revision must come from the trusted build result; a checksum downloaded beside an untrusted binary does not establish provenance.

| Host installation | Value |
| --- | --- |
| Playbook | `ansible/infra-cli.yml`; inventory group `infra_cli_targets` |
| Required variables | `infra_binary_url`, `infra_binary_revision`, `infra_binary_sha256` |
| Host cache | `/var/cache/infra/<revision>/linux-<architecture>/<sha256>/infra` |
| Active command | `/usr/local/bin/infra` symlink |
| Host recovery | `control_backup` includes `infra_binary` before enabling its service |

```sh
infra operations enrollment create-deliver --node fredrir-07 --role control --host fredrir-07
infra operations enrollment revoke-unused --node fredrir-07 --role control \
  --host fredrir-07 --key-id "$UNUSED_KEY_ID"
```

Enrollment requires the verified binary on the SSH target before creating a key; non-root remote execution uses noninteractive sudo, and key material travels only through standard input.

| Cache | Stores | Trust boundary |
| --- | --- | --- |
| Compiled binary | Executable for one revision/platform | Trusted artifact identity; no compiler on consumers |
| Bazel | Content-addressed action outputs | Separate writable trusted builds from read-only untrusted builds |
| Dagger | Container execution and filesystem results | Dedicated engine per trust boundary |
| Garage | Rust compiler/target cache and SDK objects | Existing project and release scoped credentials |

```sh
infra pipeline affected --base "$BASE_REVISION"
infra pipeline check --remote-cache "$BAZEL_REMOTE_CACHE" --read-only-cache
infra pipeline check --remote-executor "$BAZEL_REMOTE_EXECUTOR"
```

## Execution boundaries

| Workload | Execution |
| --- | --- |
| Bootstrap | GitHub-hosted runner; pinned Bazel; no dependency on an existing `infra` binary |
| Initial Dagger builds | GitHub-hosted isolated job engine |
| Production VM pool | Requires native qualification and verified replacement image digests |
| Nested Bazel actions | Process sandbox inside the Dagger container |
| Kata guest build | Dedicated isolated engine VM; explicit privileged-guest opt-in |
| Runtime activation | Separate Ansible operation after native qualification |

Production image pins, admission rules and caller commands must be promoted together; rebuilding source alone does not replace deployed images.

```sh
infra platform promote-tools --image "$VERIFIED_TOOLS_IMAGE"
infra platform promote-tools --image "$VERIFIED_TOOLS_IMAGE" --apply
git diff -- platform
```

`promote-tools` pairs the published tools digest with native commands, removes the obsolete script ConfigMap generators and deletes their four source scripts; it does not publish images or apply cluster resources.

| Dedicated VM setting | Value |
| --- | --- |
| Playbook | `ansible/build-engines.yml`; inventory group `build_engines` |
| Activation gates | `build_engine_dedicated=true`, `build_engine_qualified=true` |
| Excluded hosts | Existing `k3s_cluster` inventory |
| Prerequisite | Docker; active GitHub runner restricted to `fredrir/infra`, label `dagger-amd64` |
| Runner service | `build_engine_runner_service=actions.runner.<name>.service`; Docker access checked by Ansible |
| Workflow opt-in | Repository variable `INFRA_VM_POOL_QUALIFIED=true`; protected infrastructure `main` only |
| Dagger resource ceiling | Two CPUs, 4 GiB RAM, no swap, 256 processes |
| Dagger connection | Local `docker-container://infra-dagger`; no published engine port |
| Bazel cache | `127.0.0.1:9092`; VM-local trust boundary |
| Persistent caches | Separate Docker volumes for Dagger and Bazel; engine-local Bazel action cache |
| Action cache collection | 8 GiB / 7 days; entries accessed within 30 minutes retained |
| Cache collection | 20 GiB per cache; Dagger additionally targets 10 GiB free disk |
| Pool isolation | Trusted jobs only; untrusted PR jobs use hosted isolated engines |

```sh
ansible-playbook -i "$BUILD_ENGINE_INVENTORY" ansible/build-engines.yml \
  -e build_engine_dedicated=true -e build_engine_qualified=true \
  -e "build_engine_runner_service=$GITHUB_RUNNER_SERVICE"
```

Cache collection thresholds are garbage-collection targets, not filesystem quotas; provision enough VM disk for active builds and both caches.

The VM-local Bazel endpoint permits writes; a client read-only flag does not enforce a security boundary. Runner registration and credentials must be configured before applying the engine role. Pull requests and external reusable-workflow callers remain hosted.

## Consumer cutover

| Boundary | Required evidence |
| --- | --- |
| External workflow pins | [Consumer inventory](../build/consumers.json); all callers use the accepted infrastructure revision |
| Runner and tools images | Published immutable digests; smoke checks and provenance verification |
| Native runtime | Candidate archive and passing native qualification report |
| Retirement | No remaining callers of the old runner pools, scripts or cache service |
| Rollback | Retained accepted image digests and cache data until the rollback window closes |

The inventory records GitHub reads on its `checked` date; refresh it before retirement. See [rollout and retention](rollout.md).

## Code quality

| Rule | Check |
| --- | --- |
| Thin CLI | Operations live in internal packages |
| Deterministic tests | Temporary files, fixture HTTP servers, injected command runners |
| Failure coverage | Corruption, cancellation, trust boundaries, recovery, partial outputs |
| Dependencies | Pinned modules; generated Bazel targets checked for drift |
| Native boundaries | Bounded Linux smoke checks; explicit qualification evidence |
| Secret handling | No credentials in process errors or command-line logs |
| Changes | Tested coherent commits; independent package review |
