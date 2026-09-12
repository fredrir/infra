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
| Updated Ubuntu Noble guest | Build and archive validation passed | 124-package inventory and retained-file mapping; 177 seconds cumulative, 1.07 GiB peak; security applicability review remains open |
| Kata runtime | Not executed | KVM capability and dependency compilation do not establish Kata, Nix, BuildKit, ARC or ARM acceptance |

Nix's [ordinary sandbox setup](https://github.com/NixOS/nix/blob/2.35.2/src/libstore/linux/build/linux-derivation-builder.cc#L625) requires an ioctl absent from the pinned [gVisor implementation](https://github.com/google/gvisor/blob/release-20260907.0/pkg/sentry/socket/netstack/netstack.go#L3541). More capabilities cannot implement it. The KVM test follows the [Linux KVM API](https://docs.kernel.org/virt/kvm/api.html).

## Candidate dependency review

| Component | Official Kata 4.1.0 bundle | Candidate change | Finding |
| --- | --- | --- | --- |
| Kata runtime/agent | 4.1.0 | Retain release source | Published Kata advisories list fixes through 4.1.0; complete dependency/SBOM review remains required |
| QEMU | 11.0.1 | Standard upstream 11.0.4 through Kata's build recipe | 11.0.4 includes a [virtio-rng host use-after-free fix](https://github.com/qemu/qemu/commit/88a0e5e45b41d407cf9adf1f2cf6f20f394a578d); Kata's default QEMU runtime attaches this device |
| Guest Linux | 6.18.35 | Current 6.18 LTS, 6.18.51 on the evidence date | Keep Kata's kernel configuration and upstream recipe; verify configuration and security deltas |
| Cloud Hypervisor | 51.1 | Exclude from QEMU pilot | Affected by [CVE-2026-45782](https://github.com/cloud-hypervisor/cloud-hypervisor/security/advisories/GHSA-f47p-p25q-83rh), fixed in 51.2/52.0 |
| virtiofsd | 1.14.0 | Review unchanged active dependency | Host helper and its dependencies remain part of the isolation boundary |
| Ubuntu guest userland | Base Noble packages; package database stripped | Rebuild from fixed signed base, updates and security snapshot; retain inventory before trim | Observed libc `2.39-0ubuntu8` predates the [CVE-2024-2961 fix](https://ubuntu.com/security/CVE-2024-2961); affected conversion module is present |
| Temporary containerd | Separate candidate 2.3.5 | Private socket/root/state; CRI disabled | No K3s or host-daemon configuration change |
| Ubuntu QEMU alternative | Candidate `1:10.2.1+ds-1ubuntu3.2` on fredrir-09 | Not selected | Ubuntu lists CVE-2026-50624 as [needs evaluation](https://ubuntu.com/security/CVE-2026-50624); a lower upstream number alone neither proves nor disproves a security backport |

Sources: [Kata dependency versions](https://github.com/kata-containers/kata-containers/blob/4.1.0/versions.yaml), [Kata advisories](https://github.com/kata-containers/kata-containers/security/advisories), [QEMU stable releases](https://www.qemu.org/download/), [Linux releases](https://www.kernel.org/), [containerd release](https://github.com/containerd/containerd/releases/tag/v2.3.5).

Updating dependency versions through Kata's documented build scripts differs from maintaining Nix/gVisor source patches. It still creates a downstream bundle that needs immutable hashes, a complete active-component inventory, security review and compatibility tests. No candidate bundle is cleared for execution or production by this document.

| Recipe check | Result |
| --- | --- |
| QEMU 11.0.4 | Pinned release recipe and builder image; only an empty version-specific packaging marker is added |
| Linux 6.18.51 | Kata's existing `fs/dax` patch applies; its virtio-fs patch is already upstream and is omitted only after hash and reverse-dry-run verification |
| Build budget | Sequential native amd64 builds, two CPUs, 4 GiB memory, 15 minutes cumulative per component; QEMU reached 4 GiB without OOM and completed within budget |
| Pilot budget | One VM on fredrir-09, 2 CPU/4 GiB workload inside a 2 CPU/6 GiB host scope, 15-minute deadline |
| Build artifacts | Private pinned recipe, proposal, scope configuration and evidence under `.infra/ci-kata-pilot/`; no executable host installer |

## Proposed boundary

| Name | Value |
| --- | --- |
| Host | fredrir-09; KVM instruction execution verified |
| Runtime scope | Temporary root-owned directory, separate Unix socket and transient systemd resource boundary |
| Host integration | No service enablement, system runtime configuration, module changes, TCP listeners or K3s enrollment |
| Build guest | Standard Linux kernel; separate build profile with explicit guest capabilities and syscall policy |
| Devices | KVM/vhost interfaces used by the host runtime; no host devices passed into job containers |
| Credentials | Offline diagnostic has none; a later ARC build job still has scoped GitHub/artifact credentials |
| Storage | Private image/rootfs/workspace only; no host Nix store, Docker socket or production volumes |
| Execution limits | One VM; explicit CPU, memory, process, workspace and deadline bounds in the reviewed private proposal |
| Cleanup | Stop private tasks and runtime; remove only owned state/mounts; retain source hashes and logs |
| Mixed fleet | CI only on verified eligible workers; production remains schedulable on other workers |
| ARM | Separate hardware/runtime/native-build proof; no emulation fallback |

Kata's [static sizing](https://github.com/kata-containers/kata-containers/blob/4.1.0/docs/how-to/how-to-size-sandbox-overhead-runtime-rs.md) uses workload limits plus guest overhead. Kubernetes [Pod Overhead](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-overhead/) must additionally account for host runtime/helper memory and CPU. Reserve build requests equal to limits for the first pilot; measure combined production and CI load before relaxing that reservation.

## Acceptance gates

| Gate | Required evidence |
| --- | --- |
| Bundle | Patched standard dependency versions, complete active-file hashes/SBOM, reviewed advisories and verified build provenance |
| VM | Real Kata guest boot using KVM; no software emulation |
| Nix | Unmodified 2.35.2 ordinary derivation with sandbox and syscall filtering enabled; separate build UID and immutable inputs |
| BuildKit | Full process sandbox; real `RUN`, numeric ownership, non-root image user and OCI export |
| Host boundary | No host files, sockets, devices, production credentials or unintended listeners visible to job containers |
| Lifecycle | Cancellation/timeout kills the entire private process tree and removes owned mounts/state |
| ARC/K3s | Actual runner hooks, action/service Pods, workspace transport, admission and network policy |
| Capacity | VM and host-helper overhead, disk I/O, aggregate multi-repository progress and production latency |
| Activation | Separate reviewed runtime choice and successful native-architecture evidence |
