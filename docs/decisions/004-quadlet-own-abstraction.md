# ADR 004: Quadlet via Our Own Tiny Nix Abstraction

**Status**: Accepted · **Date**: 2026-08-09

## Context

Application containers run under Podman as **Quadlets** — systemd-native `.container`/`.network` units — a decision carried over from the backend's phase plans. NixOS has no first-party Quadlet support: `virtualisation.oci-containers` generates plain systemd services around root-level podman, and the community `quadlet-nix` flake wraps Quadlet in its own option tree. The owner wants Quadlet fidelity **without third-party abstraction**: ordinary Quadlet files, expressed as Nix-generated text.

## Decision

A small in-repo module (`modules/quadlet/`, "mkQuadlet") generates literal Quadlet files via `environment.etc`:

```nix
environment.etc."containers/systemd/users/${toString uid}/llunde-backend.container".text = ''
  [Container]
  Image=ghcr.io/fredrir/llunde-backend:latest
  Network=llunde-backend.network
  PublishPort=127.0.0.1:8080:8080
  AutoUpdate=registry

  [Service]
  Restart=always

  [Install]
  WantedBy=default.target
'';
```

- **Rootless per-user placement** uses podman's `/etc/containers/systemd/users/<uid>/` search path (podman ≥ 4.8; nixpkgs ships 5.x) — system-managed files, user-level execution, which is exactly what the per-service-user model needs ([ADR 005](005-per-service-users-isolation.md)).
- Besides the unit text, the abstraction declares everything a service user needs to run it: the user itself, `subuid`/`subgid` ranges, `users.users.<name>.linger = true`, and the per-user `podman-auto-update.timer` (deployment is pull-based, [ADR 010](010-shared-ci-reusable-workflows.md)).
- The generated text stays **plain Quadlet** — anything readable in the Quadlet man page is readable in this repo; debugging on-host compares 1:1 with upstream docs.

## Alternatives considered

- **quadlet-nix (community flake)** — closest to this design, rejected: an extra flake input and an option-tree abstraction between the operator and the unit files, for functionality a few lines of `environment.etc` provide.
- **`virtualisation.oci-containers`** — first-party but root-level podman and no Quadlet files; weakens the per-user isolation model and diverges from the decided runtime idiom.
- **Home-manager per user** — a much bigger machine to swallow for what is, per user, a directory of text files.

## Consequences

- Zero third-party Nix dependencies for the container layer; the flake's inputs stay `nixpkgs` + `disko` + `sops-nix`.
- **Known wrinkle, owned in implementation**: Quadlet regenerates units at user-manager startup; a changed `.container` file after `nixos-rebuild switch` needs a `systemctl --user daemon-reload` (+ restart) hook per affected user, or the change waits for the next session/boot. mkQuadlet grows an activation hook for this in phase 2.
- The abstraction is ours to extend (resource caps, sdnotify, health checks all pass straight through as Quadlet keys) and ours to maintain — accepted for a file format this small.
