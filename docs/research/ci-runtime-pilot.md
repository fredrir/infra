# CI runtime pilot

## Status

| Name | Value |
| --- | --- |
| Evidence date | 2026-09-12 |
| Production runtime | Unchanged; shared CI activation remains blocked |
| Architecture tested | amd64 |
| Project execution | Protected branches only; no external-PR or GitHub-hosted execution |
| Private evidence | `.infra/ci-kata-pilot/evidence/` |
| Private proposal | `.infra/ci-kata-pilot/` |

## Observed results

| Test | Result | Scope |
| --- | --- | --- |
| Pipeline image build | Passed | Rootless Podman on Archie; private image, no publication |
| gVisor `release-20260907.0`, UID 1001, no capabilities, `noNewPrivileges` | Nix namespace probe and RootlessKit UID mapping fail | Actual standalone `runsc`, `systrap`, no external network |
| Same gVisor, guest root and 16 explicit guest capabilities | BuildKit 0.33.0 passes root/non-root `RUN`, `COPY --chown=1234:2345` and OCI export | Full process sandbox, native snapshotter; diagnostic profile only |
| Nix 2.35.2, guest root, separate `nixbld` user, root-owned private store | Fails `SIOCSIFFLAGS` with `ENOTTY` | `sandbox=true`, `sandbox-fallback=false`, syscall filtering retained |
| fredrir-09 KVM | API 12; guest `MOV AX,42; HLT`; exit reason 5; AX 42 | One vCPU, 2 MiB guest-memory slot, 12 ms, no disks or network; resources released |
| QEMU 11.0.4 dependency build | Passed | Native amd64; required-device/object checks and archive validation passed; about 675 seconds across bounded attempts; cleanup verified |
| Linux 6.18.51 dependency build | Passed | Native amd64; pinned Kata configuration 202, image/ELF hashes and relative links validated; 302 seconds; cleanup verified |
| Updated Ubuntu Noble guest | Build and archive validation passed | 124-package inventory and retained-file mapping; 373.94 seconds cumulative including service/policy corrections; latest correction 99.55 seconds/1.23 GiB peak; cleanup verified |
| virtiofsd 1.14.0 dependency build | Passed | Unchanged source and Cargo.lock; pinned Rust/Alpine/native packages, link map and active library hashes; 86 seconds, 1.74 GiB peak; cleanup verified |
| Kata attempt 1 | Preflight failed; no VM | Tailscale executable path corrected; scope cleanup confirmed |
| Kata attempt 2 | QEMU/KVM started; agent RPC failed after 10 seconds | 40.47 seconds total, 4.38 GiB peak and 47 tasks; Nix/BuildKit did not run; no residual processes/mounts, services and listeners unchanged |
| Kata attempt 3 | Linux boot console captured; agent deadline expired | 34.65 seconds total, 4.27 GiB peak/48 tasks; 111 records without truncation; no captured panic or fatal QEMU error; collector and VM cleaned |
| Kata attempt 4 | Linux/systemd booted; missing Kata systemd target caused rescue mode | 60-second connection deadline; 84.78 seconds total, 4.31 GiB peak/49 tasks; no Nix/BuildKit execution; owned processes/mounts removed |
| Kata attempt 5 | Corrected agent service starts; guest powers off before RPC | 84.11 seconds total, 4.23 GiB peak/47 tasks; exit reason not captured; no Nix/BuildKit execution; complete cleanup and unchanged host snapshots |
| Kata attempt 6 | Missing default agent policy confirmed; agent exits with `SIGABRT` | Passive console records missing `/etc/kata-opa/default-policy.rego`; 86.07 seconds total, 4.24 GiB peak/48 tasks; no Nix/BuildKit execution; full cleanup and unchanged host snapshots |
| Kata attempt 7 | Nix sandbox proof passed; BuildKit blocked before Dockerfile RUN | Agent RPC connected; native BuildKit worker and COPY steps passed; session-keyring creation returned `EPERM`; 159.61 seconds, 5.08 GiB peak/50 tasks; outer cleanup confirmed |
| Kata attempt 8 | Keyring and Nix sandbox proofs passed; BuildKit blocked before RUN | Guest cgroup-device BPF query returned `EPERM`; 161.61 seconds, 5.06 GiB peak/51 tasks; native teardown `EBUSY`, outer cleanup confirmed |

Nix's [ordinary sandbox setup](https://github.com/NixOS/nix/blob/2.35.2/src/libstore/linux/build/linux-derivation-builder.cc#L625) requires an ioctl absent from the pinned [gVisor implementation](https://github.com/google/gvisor/blob/release-20260907.0/pkg/sentry/socket/netstack/netstack.go#L3541). More capabilities cannot implement it. The KVM test follows the [Linux KVM API](https://docs.kernel.org/virt/kvm/api.html).

Attempt 4 preserved the service-snapshot difference: `fwupd.service` independently deactivated successfully during the run. Pilot cleanup found no remaining owned processes or mounts, and host listeners were unchanged.

The retained agent enables its policy feature. Attempt 6 confirmed that the rebuilt guest omitted the default policy file and the agent aborted before RPC startup. The proposed correction uses the [pinned upstream policy installer](https://github.com/kata-containers/kata-containers/blob/ddcb1ad8d23cbb4323f86c209f132b89592902df/tools/osbuilder/rootfs-builder/rootfs.sh#L866) and its unchanged `allow-all.rego` for the fixed trusted offline diagnostic. That policy permits all listed guest-agent RPCs, including policy replacement; production acceptance remains separate. The approved correction rebuilt successfully in 99.55 seconds and is assembled. Archive comparison confirms exactly the three policy additions, unchanged agent/package/service bytes, and only generated hostname/creation metadata changes. Attempt 7 connected the agent and passed the ordinary Nix derivation with exact build-user mappings and immutable-input assertions. BuildKit completed COPY operations, then nested runc failed to create its session keyring before RUN; the reviewed guest seccomp profile explicitly denies keyring syscalls. A separately reviewed three-command guest keyring exception passed in attempt 8. BuildKit then reached cgroup-device BPF setup and failed because the guest profile still denies `bpf`; no BPF exception or retry followed.

## Remaining compatibility work

| Finding | Proposed work | State |
| --- | --- | --- |
| Nested BuildKit device filters | Review guest BPF commands 5/8/9/13/15/16, including replacement and metadata probes; MAP_CREATE denial is tolerated | Source analysis only; no policy change or VM |
| BPF authority | Scalar seccomp cannot restrict pointed-to program type, bytecode or target cgroup; the permission is broader than device filters | Guest kernel only; no host BPF access; built guest has BPF JIT disabled |
| Native Kata `EBUSY` | Early verified shim/virtiofsd relocation to an owned leaf under the same bounded ancestor, before automatic cleanup | Separate unimplemented harness proposal; not a production runtime fix |
| Acceptance | Full RUN/export/device/process checks and successful lifecycle remain required | Shared CI activation stays blocked |

The pinned [device-filter implementation](https://github.com/opencontainers/cgroups/blob/e0c56cb31dcb3cb2b3d1554b20dd01ced32e2a2b/devices/ebpf_linux.go) queries, loads, attaches/replaces and cleans up programs. [Cilium probes](https://github.com/cilium/ebpf/blob/159dff1788cbfd31fcf301c9abaca55336596174/syscalls.go) include non-device program types. [Seccomp cannot dereference attributes](https://docs.kernel.org/userspace-api/seccomp_filter.html), so a command allowlist cannot establish device-only BPF authority. Private source inventories and proposals are under `.infra/ci-kata-pilot/bpf-candidate/` and `cgroup-lifecycle-candidate/`.

## Candidate dependency review

| Component | Official Kata 4.1.0 bundle | Candidate change | Finding |
| --- | --- | --- | --- |
| Kata runtime/agent | 4.1.0 | Retain hash-verified release runtime-rs/agent | Published Kata advisories list fixes through 4.1.0; assembled launcher review passed; actual build compatibility remains open |
| QEMU | 11.0.1 | Standard upstream 11.0.4 through Kata's build recipe | 11.0.4 includes a [virtio-rng host use-after-free fix](https://github.com/qemu/qemu/commit/88a0e5e45b41d407cf9adf1f2cf6f20f394a578d); Kata's default QEMU runtime attaches this device |
| Guest Linux | 6.18.35 | Current 6.18 LTS, 6.18.51 on the evidence date | Keep Kata's kernel configuration and upstream recipe; verify configuration and security deltas |
| Cloud Hypervisor | 51.1 | Exclude from QEMU pilot | Affected by [CVE-2026-45782](https://github.com/cloud-hypervisor/cloud-hypervisor/security/advisories/GHSA-f47p-p25q-83rh), fixed in 51.2/52.0 |
| virtiofsd | 1.14.0 | Rebuild unchanged source/Cargo.lock with pinned builder/native packages | Link map and installed package checksums identify actual musl/libseccomp/libcap-ng dependencies; reviewed for fixed trusted diagnostic inputs |
| Ubuntu guest userland | Base Noble packages; package database stripped | Rebuild from fixed signed base, updates and security snapshot; retain inventory before trim | Observed libc `2.39-0ubuntu8` predates the [CVE-2024-2961 fix](https://ubuntu.com/security/CVE-2024-2961); affected conversion module is present |
| Temporary containerd | Separate candidate 2.3.5 | Private socket/root/state; CRI disabled | No K3s or host-daemon configuration change |
| Ubuntu QEMU alternative | Candidate `1:10.2.1+ds-1ubuntu3.2` on fredrir-09 | Not selected | Ubuntu lists CVE-2026-50624 as [needs evaluation](https://ubuntu.com/security/CVE-2026-50624); a lower upstream number alone neither proves nor disproves a security backport |

Sources: [Kata dependency versions](https://github.com/kata-containers/kata-containers/blob/4.1.0/versions.yaml), [Kata advisories](https://github.com/kata-containers/kata-containers/security/advisories), [QEMU stable releases](https://www.qemu.org/download/), [Linux releases](https://www.kernel.org/), [containerd release](https://github.com/containerd/containerd/releases/tag/v2.3.5).

Updating dependency versions through Kata's documented build scripts differs from maintaining Nix/gVisor source patches. It still creates a downstream bundle that needs immutable hashes, a complete active-component inventory, security review and compatibility tests. The reviewed private bundle started QEMU with KVM, but agent RPC failed before the build diagnostic. A 60-second connection diagnostic exposed a guest packaging omission: the raw-agent-binary install path omitted upstream systemd service files, so the guest entered rescue mode. The correction uses pinned upstream `make install-services`; its bounded rebuild passed in 97.21 seconds. Independent review verified the six intended additions, unchanged package versions and agent bytes, and valid targets/prerequisites/enablement links. Attempt 6 used passive console capture and identified the next packaging omission: the default policy file. The subsequent rebuild installs the exact upstream policy and symlink; its immutable bundle ran in attempt 7. Attempt 8 passed the corrected guest keyring check and Nix proof; BuildKit RUN/export remains blocked by the guest BPF denial. Kata reported cgroup removal `EBUSY`, while the independent outer cleanup confirmed no remaining owned processes, mounts or roots and an inactive scope. Production acceptance is a separate decision.

| Recipe check | Result |
| --- | --- |
| QEMU 11.0.4 | Pinned release recipe and builder image; only an empty version-specific packaging marker is added |
| Linux 6.18.51 | Kata's existing `fs/dax` patch applies; its virtio-fs patch is already upstream and is omitted only after hash and reverse-dry-run verification |
| Build budget | Sequential native amd64 builds, two CPUs, 4 GiB memory, 15 minutes cumulative per component; QEMU reached 4 GiB without OOM and completed within budget |
| Pilot budget | One VM on fredrir-09, 2 CPU/4 GiB workload inside a 2 CPU/6 GiB host scope, 15-minute deadline |
| Build artifacts | Private pinned recipes, assembled bundle, reviewable temporary launcher and evidence under `.infra/ci-kata-pilot/`; no runtime installation; failed diagnostic VM cleaned |

## Proposed boundary

| Name | Value |
| --- | --- |
| Host | fredrir-09; KVM instruction execution verified |
| Runtime scope | Temporary root-owned directory, separate Unix socket and transient systemd resource boundary |
| Host integration | No service enablement, system runtime configuration, module changes, TCP listeners or K3s enrollment |
| Build guest | Standard Linux kernel; separate build profile with explicit guest capabilities and syscall policy |
| Devices | KVM/vhost interfaces used by the host runtime; no host devices passed into job containers |
| Credentials | Offline diagnostic has none; a later ARC build job still has scoped GitHub/artifact credentials |
| Storage | Full private host root with detached old root; separate private job rootfs/workspace; host homes, SSH, evacuation credentials, Nix store, sockets and production volumes excluded |
| Execution limits | One VM; explicit CPU, memory, process, workspace and deadline bounds in the reviewed private proposal |
| Cleanup | Stop private tasks and runtime; remove only owned state/mounts; retain source hashes and logs |
| Mixed fleet | CI only on verified eligible workers; production remains schedulable on other workers |
| ARM | Separate hardware/runtime/native-build proof; no emulation fallback |

Kata's [static sizing](https://github.com/kata-containers/kata-containers/blob/4.1.0/docs/how-to/how-to-size-sandbox-overhead-runtime-rs.md) uses workload limits plus guest overhead. Kubernetes [Pod Overhead](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-overhead/) must additionally account for host runtime/helper memory and CPU. Reserve build requests equal to limits for the first pilot; measure combined production and CI load before relaxing that reservation.

## Acceptance gates

| Gate | Required evidence |
| --- | --- |
| Bundle | Patched standard dependency versions, active-file/package inventory, reviewed advisory applicability and verified build provenance; scanner matches alone are not confirmed reachable flaws |
| VM | Real Kata guest boot using KVM; no software emulation |
| Nix | Unmodified 2.35.2 ordinary derivation with sandbox and syscall filtering enabled; separate build UID and immutable inputs |
| BuildKit | Full process sandbox; real `RUN`, numeric ownership, non-root image user and OCI export |
| Host boundary | No host files, sockets, devices, production credentials or unintended listeners visible to job containers |
| Lifecycle | Cancellation/timeout kills the entire private process tree and removes owned mounts/state |
| ARC/K3s | Actual runner hooks, action/service Pods, workspace transport, admission and network policy |
| Capacity | VM and host-helper overhead, disk I/O, aggregate multi-repository progress and production latency |
| Activation | Separate reviewed runtime choice and successful native-architecture evidence |
