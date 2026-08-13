# Stack notes — mechanics the migration was designed against

> **Historical.** Research notes from 2026-08-09, before implementation. Several
> items were open questions then and were settled empirically during the build;
> the estate as it runs is described by the [repo README](../../README.md),
> [docs/runbook.md](../runbook.md) and the modules themselves. Kept as the
> reasoning behind those modules.

## Rootless Quadlet without third-party flakes ([ADR 004](../decisions/004-quadlet-own-abstraction.md))

- Quadlet reads per-user units from `~/.config/containers/systemd/` **and** from
  `/etc/containers/systemd/users/<UID>/` (all-users variant:
  `/etc/containers/systemd/users/`). That `/etc` path is what lets plain NixOS
  `environment.etc` entries define rootless units with no home-manager and no
  third-party flake. Requires podman ≥ 4.8; nixpkgs ships podman 5.x.
- The generator runs when the *user's* systemd instance starts ⇒ each service
  user needs `users.users.<name>.linger = true` plus declarative
  `subuid`/`subgid` ranges.
- **The wrinkle that shaped everything downstream**: `nixos-rebuild switch`
  rewrites the etc files but does not restart the affected user services.
  Something has to `systemctl --user daemon-reload` and restart what changed,
  per affected user — today that is the gitops-pull reconcile map.
- Auto-update: units carry `Label=io.containers.autoupdate=registry`; each
  service user enables `podman-auto-update.timer` (user unit). Rollback on
  failed health is `podman auto-update --rollback` semantics with `HealthCmd`
  set ([ADR 010](../decisions/010-shared-ci-reusable-workflows.md)).

## Rootless networking ([ADR 005](../decisions/005-per-service-users-isolation.md))

- Podman networks are **per-user**; containers of different users can never
  share one. Cross-user traffic = loopback-published ports only: backend API
  `127.0.0.1:8080`, frontend `127.0.0.1:8081`, Caddy the sole public listener.
- The `edge` user binds 80/443 rootlessly via
  `boot.kernel.sysctl."net.ipv4.ip_unprivileged_port_start" = 80`.
- Inside the `llunde-backend` user, backend↔postgres↔valkey share one private
  podman network; only the API port is loopback-published.

## nixos-anywhere + Disko ([ADR 001](../decisions/001-nixos-declarative-host.md))

- nixos-anywhere kexecs into an installer from the running Ubuntu box over SSH,
  applies the Disko layout, installs the flake's system, reboots. Everything on
  disk is destroyed — the accepted openclaw/old-stack wipe.
- `--extra-files` injects files before first boot — used to place the
  **pre-generated SSH host key** so the sops-nix age recipient is stable and
  secrets decrypt on first boot (generate the host key locally, derive its age
  key, encrypt to it, inject the key).

## sops-nix bootstrap ([ADR 007](../decisions/007-sops-nix-bootstrap.md))

- Age recipients derived from the host's `ssh-ed25519` host key (`ssh-to-age`);
  operator key(s) as additional recipients for editing. Secrets decrypt at
  activation into `/run/secrets/<name>` with per-secret owner/group.
- Three bootstrap secrets were planned (Doppler service token, Tailscale auth
  key, restic repository password); the set grew with the estate — current list
  in [secrets/README.md](../../secrets/README.md).

## OpenTofu migration ([ADR 002](../decisions/002-opentofu-s3-state.md))

Tofu is state-compatible with Terraform; migration is `tofu init` against
existing state. Backend move: add the `s3` backend block, `tofu init
-migrate-state`, then a **steady-state `plan` showing "No changes"** as the
proof — mandatory where real data lives. Provider lock files regenerate under
tofu.

## Resource caps on the shared 4 GB box ([ADR 005](../decisions/005-per-service-users-isolation.md))

Declared in the quadlet units, not left to defaults: JVM `-Xmx` (~512–768 MB),
Postgres `shared_buffers` (~256 MB), Valkey `maxmemory` (with `appendonly yes`
retained), and awareness that argon2id burns 19 MiB × concurrent logins inside
the JVM budget. `MemoryMax=` gives systemd-level backstops. Rescale to 8 GB is a
CPU/RAM-only Hetzner rescale — avoid the irreversible disk grow.

## Cloudflare proxy interaction ([ADR 012](../decisions/012-cloudflare-strategy.md))

The backend trusts only the **rightmost** `X-Forwarded-For` entry (Ktor
`useLastProxy()`; RFC-7239 `Forwarded` ignored entirely), and its ASVS
anti-automation checklist row is conditional on topology: app port reachable
only via Caddy, proxy strips inbound forwarding headers then appends the real
client IP. Putting Cloudflare in front inserts a hop, so CF must be resolved by
**Caddy** (`trusted_proxies` with Cloudflare ranges) or rate-limit keys and
audit IPs silently become CF datacenter addresses. Verified with the
rotating-XFF test before and after.
([ADR 017](../decisions/017-tunnel-ingress-llunde.md) later replaced direct
proxying with tunnel ingress; the header chain is the same problem.)
