# Development

## Architecture

| Layer                     | Owner                                 | Contract                                                          |
| ------------------------- | ------------------------------------- | ----------------------------------------------------------------- |
| Commands                  | `cmd/infra`, `internal/cli`           | Cobra flags, environment conversion, cancellation, output         |
| Infrastructure operations | `internal/ci`, `internal/platformops` | Typed inputs; injectable process and HTTP boundaries              |
| Runtime artifacts         | `internal/kata`                       | Pinned sources; verified downloads; native qualification          |
| Builds                    | Bazel                                 | Explicit dependency graph; pinned Go SDK; reusable actions        |
| Container execution       | Dagger Go SDK                         | Compiled into `infra`; pinned engine; separate execution cache    |
| Binary installation       | `internal/artifact`                   | Exact revision, platform and trusted SHA-256; atomic installation |
| Process lifecycle         | `internal/process`                    | Context cancellation; process-group termination; bounded capture  |
| Object storage            | `internal/objectstore`                | S3 request signing; bounded artifact transfer                     |
| Resource ownership        | OpenTofu, Ansible, Flux               | Provider resources, host configuration, cluster reconciliation    |

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

| Setting                       | Source                                             |
| ----------------------------- | -------------------------------------------------- |
| Go dependencies               | `go.mod`, `go.sum`                                 |
| Bazel dependencies            | `MODULE.bazel`, `MODULE.bazel.lock`                |
| Bazel executable              | `.bazelversion`, `build/toolchain.json`            |
| Dagger engine and build image | `build/toolchain.json`                             |
| Command discovery             | `infra --help`; `infra <command> --help`           |
| Human progress                | Standard error                                     |
| Structured results            | Standard output; command-specific JSON             |
| Build evidence                | Result JSON, Bazel build events and timing profile |

## Binary reuse

| Consumer                 | Binary identity                                   | Cache                                                     |
| ------------------------ | ------------------------------------------------- | --------------------------------------------------------- |
| Reusable GitHub workflow | `job.workflow_sha`, Linux amd64, pinned toolchain | Exact-key binary cache; verified workflow artifact        |
| Workstation or host      | Full revision, OS/architecture, trusted SHA-256   | Private user cache; checksum revalidated on every install |
| Container image          | Verified precompiled Linux binary                 | Image layers; digest-pinned deployment                    |

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

| Host installation  | Value                                                                |
| ------------------ | -------------------------------------------------------------------- |
| Playbook           | `ansible/infra-cli.yml`; inventory group `infra_cli_targets`         |
| Required variables | `infra_binary_url`, `infra_binary_revision`, `infra_binary_sha256`   |
| Host cache         | `/var/cache/infra/<revision>/linux-<architecture>/<sha256>/infra`    |
| Active command     | `/usr/local/bin/infra` symlink                                       |
| Host recovery      | `control_backup` includes `infra_binary` before enabling its service |

```sh
infra operations enrollment create-deliver --node fredrir-07 --role control --host fredrir-07
infra operations enrollment revoke-unused --node fredrir-07 --role control \
  --host fredrir-07 --key-id "$UNUSED_KEY_ID"
```

Enrollment requires the verified binary on the SSH target before creating a key; non-root remote execution uses noninteractive sudo, and key material travels only through standard input.

| Cache           | Stores                                     | Trust boundary                                                   |
| --------------- | ------------------------------------------ | ---------------------------------------------------------------- |
| Compiled binary | Executable for one revision/platform       | Trusted artifact identity; no compiler on consumers              |
| Bazel           | Content-addressed action outputs           | Separate writable trusted builds from read-only untrusted builds |
| Dagger          | Container execution and filesystem results | Dedicated engine per trust boundary                              |
| Garage          | Rust compiler/target cache and SDK objects | Existing project and release scoped credentials                  |

```sh
infra pipeline affected --base "$BASE_REVISION"
infra pipeline check --remote-cache "$BAZEL_REMOTE_CACHE" --read-only-cache
infra pipeline check --remote-executor "$BAZEL_REMOTE_EXECUTOR"
```

## Execution boundaries

| Workload              | Execution                                                                       |
| --------------------- | ------------------------------------------------------------------------------- |
| Bootstrap             | GitHub-hosted runner; pinned Bazel; no dependency on an existing `infra` binary |
| Initial Dagger builds | GitHub-hosted isolated job engine                                               |
| Production VM pool    | Requires native qualification and verified replacement image digests            |
| Nested Bazel actions  | Process sandbox inside the Dagger container                                     |
| Kata guest build      | Dedicated isolated engine VM; explicit privileged-guest opt-in                  |
| Runtime activation    | Separate Ansible operation after native qualification                           |

Production image pins, admission rules and caller commands must be promoted together; rebuilding source alone does not replace deployed images.

```sh
infra platform promote-tools --image "$VERIFIED_TOOLS_IMAGE"
infra platform promote-tools --image "$VERIFIED_TOOLS_IMAGE" --apply
git diff -- platform
```

`promote-tools` pairs the published tools digest with native commands, removes the obsolete script ConfigMap generators and deletes their four source scripts; it does not publish images or apply cluster resources.

| Dedicated VM setting            | Value                                                                                                                                       |
| ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| Host provisioning               | `ansible/build-vms.yml`; inventory group `build_vm_hosts`                                                                                   |
| Runner provisioning             | `ansible/build-runners.yml`; inventory group `build_engines`                                                                                |
| Engine provisioning             | `ansible/build-engines.yml`; inventory group `build_engines`                                                                                |
| Production placement            | `ansible/inventory/production.yml`; `infra-build-09` on `fredrir-09`                                                                        |
| Reviewable provisioning example | `ansible/examples/build-vm-fredrir-09.yml`                                                                                                  |
| Host reservation                | 4.25 CPUs / 10 GiB reserved; resulting allocatable 11.5 CPUs / 21,585,868 KiB                                                               |
| Guest                           | Four CPUs / 8 GiB RAM / 80 GiB sparse persistent disk                                                                                       |
| Host boundary                   | Unprivileged QEMU account; KVM device; loopback-only SSH forwarding                                                                         |
| Activation gates                | `build_vm_enabled=true`, `build_engine_dedicated=true`, `build_engine_qualified=true`                                                       |
| Runner registration             | Eight repository registrations share one VM; each accepts protected main pushes or manual runs only                                         |
| Runner slice                    | Four CPUs / 2 GiB aggregate for runner services and native child processes                                                                   |
| Production Dagger ceiling       | Four CPUs / 4 GiB RAM / 1,024 processes / one parallel operation                                                                           |
| Standalone engine defaults      | Four CPUs / 8 GiB RAM / one parallel operation; configurable within host capacity                                                         |
| Dagger connection               | Local `docker-container://infra-dagger`; no published engine port                                                                           |
| Bazel cache                     | `127.0.0.1:9092`; one CPU / 1 GiB RAM; VM-local trusted writes                                                                              |
| Bazel server retention | 300 s idle timeout; repository, disk and remote action caches persist |
| Persistent caches               | Separate Docker volumes for Dagger and Bazel; engine-local Bazel action cache; native repository downloads in `/var/cache/infra/bazel-repo` |
| Runner cache environment        | `BAZEL_REMOTE_CACHE=http://127.0.0.1:9092`; `_EXPERIMENTAL_DAGGER_RUNNER_HOST=docker-container://infra-dagger`                              |
| Runner enforcement              | Immutable root-owned job hook; `CI_POOL=main`; foreign owners, PRs and unprotected refs rejected                                            |
| Cache collection                | Dagger ordinary layers first; named caches preferred for 48 h; 20 GiB target / 4 GiB emergency free space; Bazel remote 20 GiB target / 21 GiB admission ceiling                                                      |
| Background preparation          | Six-hour systemd timer; low-priority client; ten-minute timeout; configured argv only                                                       |
| Background maintenance            | `infra artifact prune-source`; `infra pipeline cache-gc`                                                                           |
| Verified tooling                | CLI release checksum and revision; pinned native Bazel; pinned GitHub runner archive                                                        |
| Warm ARC capacity               | One deploy runner and one declaration-check runner                                                                                          |
| Pool isolation                  | Trusted protected-branch jobs only; untrusted PR jobs use isolated hosted engines                                                           |

```sh
ansible-playbook -i ansible/inventory/production.yml \
  ansible/k3s.yml --limit fredrir-09 --tags capacity --check --diff
ansible-playbook -i ansible/inventory/production.yml \
  ansible/build-vms.yml --check --diff
```

| Activation order      | Required result                                                                                                           |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------- |
| Publish tooling       | `build/cli-release.json` identifies a verified release supporting preparation and cache collection                        |
| Reserve host capacity | Apply reviewed K3s reservation; verify worker readiness and allocatable resources                                         |
| Provision guest       | Apply VM playbook with `build_vm_enabled=true`; verify guest SSH host identity before adding it to known hosts            |
| Qualify guest         | Native Linux builds, persistent cache reuse, resource ceilings and trust isolation pass                                   |
| Register runners      | Apply runner playbook with `build_engine_qualified=true`; registration tokens obtained locally through authenticated `gh` |
| Route trusted work    | Enable workflow qualification variables only after runner labels and protected-branch conditions match                    |
| Verify latency        | Measure queue, setup, checks, publication and revision readiness independently                                            |
| Roll back routing     | Disable qualification variables before stopping services; retain guest disk and cache volumes                             |

The reservation and dedicated guest are provisioned and natively qualified; workflow routing remains separately gated by release identity and qualified-pool variables. The guest shares physical CPUs with Kubernetes; reservations constrain schedulable capacity rather than guaranteeing latency. Runner registrations share one capped engine, so concurrent repositories may queue. Background preparation limits its client process; expensive Dagger work remains bounded by the engine's shared limits. Cache thresholds are retention targets rather than filesystem quotas, except for Bazel remote's write-admission ceiling and the guest disk's virtual capacity.

The VM-local Bazel endpoint permits writes; a client read-only flag does not enforce a security boundary. Only trusted repositories and protected branches may use this VM. Prewarming starts only when supported commands and their working directory exist; first population may exceed the normal CI budget.

## Consumer cutover

| Boundary                | Required evidence                                                                                   |
| ----------------------- | --------------------------------------------------------------------------------------------------- |
| External workflow pins  | [Consumer inventory](../build/consumers.json); all callers use the accepted infrastructure revision |
| Runner and tools images | Published immutable digests; smoke checks and provenance verification                               |
| Native runtime          | Candidate archive and passing native qualification report                                           |
| Retirement              | No remaining callers of the old runner pools, scripts or cache service                              |
| Rollback                | Retained accepted image digests and cache data until the rollback window closes                     |

The inventory records GitHub reads on its `checked` date; refresh it before retirement. See [rollout and retention](rollout.md).

## Code quality

| Rule                | Check                                                                 |
| ------------------- | --------------------------------------------------------------------- |
| Thin CLI            | Operations live in internal packages                                  |
| Deterministic tests | Temporary files, fixture HTTP servers, injected command runners       |
| Failure coverage    | Corruption, cancellation, trust boundaries, recovery, partial outputs |
| Dependencies        | Pinned modules; generated Bazel targets checked for drift             |
| Native boundaries   | Bounded Linux smoke checks; explicit qualification evidence           |
| Secret handling     | No credentials in process errors or command-line logs                 |
| Changes             | Tested coherent commits; independent package review                   |

## Performance budgets

| Path                | Budget                     | Measurement                                                                  |
| ------------------- | -------------------------- | ---------------------------------------------------------------------------- |
| Fast checks         | 10 seconds aggregate       | `infra pipeline check-fast`; `infra ci measure`; image check receipts        |
| Deployment          | 60 seconds target          | Workflow creation through expected revision readiness, including queueing    |
| Frontend deployment | 30 seconds target          | `https://llunde.no/.well-known/revision` must match source revision          |
| Cold preparation    | Reported separately        | Dependency/toolchain compilation; never reported as a passing fast check     |
| Process resources   | Per command                | CPU seconds and subprocess maximum RSS; remote engine resources are separate |
| Cache resources     | Bounded persistent storage | VM volumes, Bazel action metrics and engine resource ceilings                |

| Local operation                         | Command                                                                                                                 |
| --------------------------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| Complete infrastructure checks          | `infra pipeline check-deep --local`                                                                                     |
| Prepare infrastructure test executables | `infra pipeline prepare-check --local`                                                                                  |
| Fast checks                             | `infra pipeline check-fast --local --base HEAD~1`                                                                       |
| Maintain Dagger action cache            | `infra pipeline cache-gc`                                                                                               |
| Rust full validation                    | `infra ci rust prepare && infra ci rust check deep`                                                                     |
| Rust cross-toolchain qualification      | `infra ci rust check toolchain` inside the verified Rust runner image                                                   |
| Full package installation matrix        | `infra packages smoke --root . SITE CHANNELS full`                                                                      |
| Measure a bounded command               | `infra ci measure --stage checks --budget 10s --report-dir /tmp/infra-performance -- COMMAND`                           |
| Inspect deployment critical path        | `infra ci timeline REPOSITORY RUN_ID --deployment-run RUN_ID --budget 30s`                                              |
| Prefetch verified image layers          | `infra platform prefetch --namespace llunde --node fredrir-09 --utility-image "$TOOLS_IMAGE" "$VERIFIED_IMAGE" --apply` |

Slow suites remain explicit local operations. Failed, missing or timed-out checks fail the gate. Preparation and qualification costs remain visible; the target budgets are not evidence of achieved end-to-end latency.

```sh
TOOLS_IMAGE=ghcr.io/fredrir/platform-backup-tools@sha256:5d350bdc39e4bf66e4db23944d63cdac7c1098eda71e9a5fde0364191b33d5e2
infra platform prefetch --namespace llunde --node fredrir-09 \
  --utility-image "$TOOLS_IMAGE" --pull-secret ghcr --timeout 30s \
  "$VERIFIED_IMAGE" --apply
```

| Timing and prefetch boundary | Behavior                                                                                                              |
| ---------------------------- | --------------------------------------------------------------------------------------------------------------------- |
| Timeline start               | GitHub API workflow creation is the earliest known timestamp, not the exact push-event timestamp                      |
| Prefetch execution           | Only the platform-tools helper executes; application images mount read-only as image volumes                          |
| Prefetch lifetime            | At most one minute; cleanup on success, failure or cancellation; finished Job TTL 60 seconds                          |
| Prefetch prerequisites       | Kubernetes 1.36 image volumes; compatible container runtime; namespace pull credentials; verified owned image digests |
| Native VM evidence           | [Warm VM qualification](../build/evidence/warm-vm-linux-amd64.json)                                                   |
| Process metrics | OS-reported command CPU and peak memory; timed-out descendants and external build services may be excluded |
| Selective GitOps | [Qualified controller and ownership handoff](../build/rollout/flux-artifacts/README.md) |

Initial native preparation is explicit; periodic compilation is disabled to avoid contending with running CI jobs.

| Image preparation input | Required match |
| --- | --- |
| Source | Git checkout at the intended revision, including executable file modes |
| Execution | CI CLI checksum, engine, platform, build arguments and build targets |
| Cache identity | Dependency bytes and file modes; archive extraction can change modes and invalidate layers |
| Qualification | Preparation is reported separately; only a measured CI run establishes deployment latency |
| Evidence | [Frontend checkout-mode diagnosis and publication breakdown](../build/rollout/flux-artifacts/rollout.json) |

Create the preparation checkout from an existing repository:

```sh
umask 022
git worktree add --detach "$PREPARATION_DIR" "$SOURCE_REVISION"
```

```sh
infra ci prepare-validation
infra ci validate
```
