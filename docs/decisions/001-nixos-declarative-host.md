# ADR 001: NixOS as the Declarative Host OS

**Status**: Accepted · **Date**: 2026-08-09

## Context

The llunde system is being rebuilt around one principle: **Git describes desired state**. The old host (`llunde-cpx22`, Hetzner 2 vCPU/4 GB, Ubuntu 24.04) accumulated state the old way — a hand-managed `/opt/llunde` docker-compose stack deployed by rsync-and-ssh CI, nginx+certbot configured in place, an unrelated `openclaw` service in `/home`, and a Terraform module that could only *adopt* the machine (`prevent_destroy` + `ignore_changes`), never rebuild it (see [../research/current-state.md](../research/current-state.md)). Nothing about the box was reproducible.

An earlier round of this design settled on idempotent shell scripts to configure the live Ubuntu box in place. The owner then explicitly superseded that: the host should be **declarative and reproducible end-to-end**, with the OS itself owned by configuration.

## Decision

The host runs **NixOS**, and the NixOS configuration in this repository is the single source of truth for everything about the machine: users, SSH policy, host firewall, sysctls, packages, systemd units, container quadlets, timers.

- **Install path**: [nixos-anywhere](https://github.com/nix-community/nixos-anywhere) installs the flake's host configuration onto the box over SSH (kexec), with **Disko** declaring the disk layout in Nix — single-disk ext4, no ZFS/btrfs ceremony on a 4 GB machine.
- **The existing box is wiped.** That destroys the old `/opt/llunde` stack, nginx, docker, *and openclaw*. The owner accepted this explicitly: openclaw is declarative in its own repository and holds nothing important. This repo does **not** redeploy openclaw; its user slot stays reserved (see [ADR 005](005-per-service-users-isolation.md)) should it ever return as a managed service.
- systemd is an intentional part of the architecture, not incidental: process supervision, timers (backups, auto-update), and quadlet generation all run through it, declared via Nix.
- Reprovisioning from a bare Hetzner server to serving traffic must be a runbook-driven, repeatable operation — the phase-2 gate proves it.

## Alternatives considered

- **Idempotent shell scripts over SSH on the live Ubuntu box** — the earlier decision; superseded. Scripts describe *actions*, not state; drift detection and rollback are manual.
- **Ansible** — real idempotence semantics, but a second toolchain and still convergence-based, not generative; the owner wants the OS itself derived from declarations.
- **Staying on Ubuntu with podman** — workable (podman 4.9 supports Quadlet) but keeps the host as a pet: every apt/config change is untracked state.

## Consequences

- The machine becomes cattle: `tofu apply` + `nixos-anywhere` + secrets bootstrap ([ADR 007](007-sops-nix-bootstrap.md)) reproduce it from nothing.
- NixOS generations give atomic switches and rollback for host changes ([ADR — apply mechanism recorded in phase plans]).
- A brief full outage of everything on the box during the wipe is accepted (the old llunde stack has no users; openclaw is expendable).
- Everyone touching the host must work through the repo — hand edits on the box are not merely discouraged, they are erased by the next rebuild.
- Related: repo shape in [ADR 003](003-repo-shape.md), container runtime in [ADR 004](004-quadlet-own-abstraction.md).
