# Stack notes — mechanics the plans rest on

Verified-where-possible mechanics underpinning the phase plans. Items marked **[verify]** must be confirmed empirically during phase 1/2 implementation rather than trusted from here.

## Rootless Quadlet without third-party flakes ([ADR 004](../decisions/004-quadlet-own-abstraction.md))

- Quadlet reads per-user units from `~/.config/containers/systemd/` **and** from `/etc/containers/systemd/users/<UID>/` (all-users variant: `/etc/containers/systemd/users/`). The `/etc/.../users/<UID>/` path is what lets plain NixOS `environment.etc` entries define rootless units with no home-manager and no third-party flake. Requires podman ≥ 4.8; current nixpkgs ships podman 5.x **[verify exact version at implementation]**.
- The quadlet generator runs when the *user's* systemd instance starts ⇒ each service user needs `users.users.<name>.linger = true` (NixOS supports declarative linger) plus declarative `subuid`/`subgid` ranges.
- **Open wrinkle**: `nixos-rebuild switch` rewrites the etc files but does not reload user managers. The `mkQuadlet` abstraction must ship an activation hook that runs `systemctl --user daemon-reload` + restarts changed units per affected user (loginctl/`machinectl shell` or `systemd-run --user --machine <user>@`) — or the plan accepts reboot/manual restart initially. **[verify chosen mechanism in phase 2]**
- Auto-update: units carry `Label=io.containers.autoupdate=registry`; each service user enables `podman-auto-update.timer` (user unit). Rollback on failed health is `podman auto-update --rollback` semantics with `HealthCmd` set. ([ADR 010](../decisions/010-shared-ci-reusable-workflows.md))

## Rootless networking facts ([ADR 005](../decisions/005-per-service-users-isolation.md))

- Podman networks are **per-user**; containers of different users can never share one. Cross-user traffic = loopback-published ports only: backend API `127.0.0.1:8080`, frontend `127.0.0.1:8081`, Caddy the sole public listener.
- The `edge` user binds 80/443 rootlessly via `boot.kernel.sysctl."net.ipv4.ip_unprivileged_port_start" = 80`.
- Inside the `llunde-backend` user, backend↔postgres↔valkey share one private podman network; only the API port is loopback-published.

## nixos-anywhere + Disko ([ADR 001](../decisions/001-nixos-declarative-host.md))

- nixos-anywhere kexecs into an installer from the running Ubuntu box over SSH, applies the Disko layout (single-disk ext4 — GPT + ESP even though Hetzner cloud boots BIOS-compatible; use the standard hetzner single-disk example **[verify boot mode on cpx22]**), installs the flake's system, reboots. Everything on disk is destroyed — the accepted openclaw/old-stack wipe.
- `--extra-files` injects files into the target before first boot — used to place the **pre-generated SSH host key** so the sops-nix age recipient is stable and secrets decrypt on first boot (generate host key locally, derive its age key, encrypt secrets to it, inject the key).

## sops-nix bootstrap ([ADR 007](../decisions/007-sops-nix-bootstrap.md))

- Age recipients derived from the host's `ssh-ed25519` host key (`ssh-to-age`); operator key(s) as additional recipients for editing. Secrets decrypt at activation into `/run/secrets/<name>` with per-secret owner/group — e.g. the Doppler token readable only by the service users' wrapper.
- Exactly three bootstrap secrets: Doppler service token(s), Tailscale auth key, restic repository password. Everything else stays in Doppler.

## OpenTofu migration ([ADR 002](../decisions/002-opentofu-s3-state.md))

- Tofu is state-compatible with Terraform; migration is `tofu init` against existing state. Backend move: add the `s3` backend block, `tofu init -migrate-state`, then a **steady-state `plan` showing "No changes"** as the proof — mandatory for pyparser in phase 3, where real data lives. Provider lock files regenerate under tofu.

## Resource caps on the shared 4 GB box ([ADR 005](../decisions/005-per-service-users-isolation.md))

Declared in the quadlet units, not left to defaults: JVM `-Xmx` (~512–768 MB), Postgres `shared_buffers` (~256 MB), Valkey `maxmemory` (with `appendonly yes` retained), and awareness that argon2id burns 19 MiB × concurrent logins inside the JVM budget. `MemoryMax=` on the units gives systemd-level backstops. Rescale to 8 GB is a later CPU/RAM-only Hetzner rescale (avoid the irreversible disk grow).

## Cloudflare orange-cloud interaction — phase 4 only ([ADR 012](../decisions/012-cloudflare-strategy.md))

The backend trusts only the **rightmost** `X-Forwarded-For` entry (Ktor `useLastProxy()`; RFC-7239 `Forwarded` ignored entirely), and its ASVS anti-automation checklist row is explicitly conditional on topology: app port reachable only via Caddy, proxy strips inbound forwarding headers then appends the real client IP. Turning on CF proxy inserts a hop: CF must be resolved by **Caddy** (`trusted_proxies` with Cloudflare ranges) so Caddy forwards the true client IP; otherwise rate-limit keys and audit IPs silently become CF datacenter addresses. Verify with the existing rotating-XFF test before and after.
