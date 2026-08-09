# Runbook — bare Hetzner server → serving llunde

Executed literally at the phase-2 go-live and re-executed on any reprovision.
**Every deviation discovered during execution is a documentation bug — fix it here, on the spot** ([phase-2 tasks 2.4](init/plans/phase-2/tasks.md)).

Values in `<ANGLE_BRACKETS>` are fill-ins; each states its source. Amounts of ceremony that look skippable are not — the order is load-bearing (secrets must exist *before* install because the host key is pre-generated and injected).

---

## 1. Prerequisites (laptop — DONE as of 2026-08-09, kept for reprovision)

Nix **is** installed natively on the Mac (`nix 2.35.1` via `~/.nix-profile`), alongside the
Nix devx tools (`nixd`, `statix`, `alejandra`, `deadnix`). Run nix commands directly —
the podman `nixos/nix` wrapper from the phase-1/2 era is no longer needed.

1. Tools (all present): `opentofu`, `sops`, `age`, `awscli` via brew; `ssh-to-age` has no
   brew formula — use `nix run nixpkgs#ssh-to-age` (or `nix profile install nixpkgs#ssh-to-age`).
   `hcloud` CLI authenticated (`hcloud server list` shows 132168416) · `doppler` logged in.
2. Owner age key (generated 2026-08-09, lives at `~/.config/sops/age/keys.txt` — **back it up**):
   public key `age13upkqrd7v97a4gcgerwnkuwvznxh60u2g56x6cj55hemwq68q3lsalynwn`
   (already set as the admin recipient in `.sops.yaml`).
3. Hetzner token for tofu (from the hcloud CLI config):
   ```sh
   export TF_VAR_hcloud_token=$(awk -F'"' '/^[[:space:]]*token/ {print $2; exit}' ~/.config/hcloud/cli.toml)
   ```
4. AWS env for the S3 state backend comes from Doppler on every tofu call: prefix commands
   with `doppler run --project pyparser --config prd --`. (An admin AWS profile also exists
   locally via `aws login` — root credentials; reserve it for phase-3 IAM work and consider
   replacing with a scoped admin user later.)

### Optional rehearsal: `nixos-dev` VM

A NixOS test VM exists (`ssh nixos-dev` — ProxyJump via the Arch desktop `archie` over
Tailscale; 4 vCPU/6 GiB, NixOS 26.05 minimal, libvirt NAT). Before wiping prod you can
rehearse there: build this flake's closure on the VM (`nixos-rebuild build --flake .#llunde-01`
after rsyncing the repo), or dry-activate individual modules. Hcloud-specific bits (disko
device names, firewall) and the flake's 25.11 pin vs the VM's 26.05 will differ — treat it
as a module test bench, not a full dress rehearsal.

## 2. Provision (tofu apply — safe pre-wipe: rename + firewall only)

```sh
cd <repo-root>
doppler run --project pyparser --config prd -- tofu -chdir=tofu/llunde apply
```

Expected plan (same set proven in phase 1): **1 to import** (`hcloud_server.llunde_01` = existing 132168416), **2 to add** (firewall `llunde-fw` 22/80/443 + attachment), **1 to change** (rename `llunde-cpx22` → `llunde-01`). Nothing here touches disk contents. Confirm `hcloud server describe 132168416 | grep -i name` shows `llunde-01`.

## 3. Pre-generate host identity & secrets (BEFORE install)

The host's SSH key is created *by us* and injected at install, so sops-decryption works from first boot (ADR 007).

1. Generate the host key locally (kept only until injected, then deleted):
   ```sh
   mkdir -p /tmp/llunde-01-keys/etc/ssh
   ssh-keygen -t ed25519 -N "" -C llunde-01 -f /tmp/llunde-01-keys/etc/ssh/ssh_host_ed25519_key
   ssh-to-age < /tmp/llunde-01-keys/etc/ssh/ssh_host_ed25519_key.pub   # -> <HOST_AGE_PUBLIC_KEY>
   ```
2. Edit `.sops.yaml`: the admin key is already real; replace `age1PLACEHOLDER_HOST_KEY` → `<HOST_AGE_PUBLIC_KEY>` (from `ssh-keyscan -t ed25519 46.62.214.182 | nix run nixpkgs#ssh-to-age`, or the pre-generated key path below).
3. Create the five secret files (`sops secrets/<name>.yaml` opens an editor; exact keys below are final, from `modules/secrets/default.nix`):
   - `secrets/doppler.yaml` — key `doppler_token`, value in **env-file form** (it lands as an EnvironmentFile):
     `DOPPLER_TOKEN=<token>` where the token comes from
     `doppler configs tokens create llunde-01 --project llunde --config prd --plain --max-age 0`
   - `secrets/tailscale.yaml` — key `auth_key`, value from the Tailscale admin console → Settings → Keys → *Auth keys* → Generate (reusable: no, ephemeral: no, tags optional).
   - `secrets/restic.yaml` — two keys: `password` (from `openssl rand -base64 32`) and `env` in env-file form:\n     `AWS_ACCESS_KEY_ID=...`, `AWS_SECRET_ACCESS_KEY=...`, `AWS_DEFAULT_REGION=eu-north-1` — **decision (recorded)**: v1 reuses the `leploy` credentials from Doppler `pyparser/prd` (object-level S3 rights suffice); a dedicated backup IAM user is a hardening follow-up (contract.md).
   - `secrets/llunde-backend-db.yaml` — key `env`, value in env-file form (contract.md): `POSTGRES_PASSWORD`/`DB_PASSWORD` = `openssl rand -base64 24` (same value), optional `VALKEY_PASSWORD`.
   - `secrets/ghcr.yaml` — key `auth_json` (images are PRIVATE): create a fine-grained PAT
     (github.com/settings/tokens → read-only `packages` scope, no repo perms needed for classic `read:packages`),
     then the value is the literal JSON:
     `{"auths":{"ghcr.io":{"auth":"$(echo -n 'fredrir:<PAT>' | base64)"}}}`
     Verify later on-host: `REGISTRY_AUTH_FILE=/run/secrets/ghcr-auth.json podman pull ghcr.io/fredrir/llunde-frontend:latest`.
4. Mirror `DB_PASSWORD` (and `VALKEY_PASSWORD` if set) into Doppler `llunde/prd` so app-level and infra views agree.
5. `git add -A && git commit -m "phase 2: real sops recipients + secrets"` (ciphertext only — verify `git diff --cached` shows only `sops`-encrypted content).

## 4. Install NixOS (💥 DESTROYS THE BOX — old stack AND openclaw; accepted in ADR 001)

Preconditions: step 2 applied; step 3 committed; you can `ssh root@46.62.214.182` (current alias `ssh llunde`).

```sh
nix run github:nix-community/nixos-anywhere -- \
  --flake .#llunde-01 \
  --build-on-remote \
  -i ~/.ssh/id_ed25519 \
  --extra-files /tmp/llunde-01-keys \
  root@46.62.214.182
```

Notes: native nix on the Mac; `--build-on-remote` is required (laptop is aarch64-darwin, target x86_64-linux); nixos-anywhere kexecs into an installer, runs disko (single-disk ext4 wipe of `/dev/sda`), installs the flake's system, copies `--extra-files` (the host key) into place, reboots. ⚠️ Verify-at-execution: exact `-i`/`--extra-files` flag spellings against the nixos-anywhere version pulled.

Afterwards: `rm -rf /tmp/llunde-01-keys` (the host key now lives only on the host). `ssh root@46.62.214.182` must present the ed25519 fingerprint you generated.

## 5. Verify first boot & tailnet join

```sh
ssh root@46.62.214.182 systemctl --failed          # expect: 0 loaded units listed
ssh root@46.62.214.182 ls /run/secrets/            # expect the four secrets materialized
ssh root@46.62.214.182 tailscale status            # expect: joined, hostname llunde-01
tailscale status | grep llunde-01                  # from the laptop -> <TAILNET_IP> (100.x.y.z)
ssh root@<TAILNET_IP> true                         # management path works (ADR 008)
```

## 6. Deploy configuration changes (steady-state loop)

Recommended (push from laptop; works while the repo is private):

```sh
git push && nixc-ssh nixos-rebuild switch --flake .#llunde-01 \
  --target-host root@<TAILNET_IP> --build-host root@<TAILNET_IP>
# nixc-ssh = the nixc alias + `-v ~/.ssh/id_ed25519:/root/.ssh/id_ed25519:ro`
```

Alternative (on-host): `nixos-rebuild switch --flake github:fredrir/llunde-infra#llunde-01` — ⚠️ requires the private repo readable from the host (fine-grained PAT in `/etc/nix/netrc`); set that up only if the push flow annoys.

After any change touching quadlet units, confirm regeneration: `ssh root@<TAILNET_IP> systemctl --user -M llunde-backend@ list-units 'llunde-*'` (matches the quadlet module's `systemctl --machine=<user>@ --user` hook).

## 7. DNS cutover (Cloudflare dashboard, zone `llunde.no` = 4ae54b24fc4140d4d1c450491645f1c8)

Precondition: `curl -H "Host: api.llunde.no" http://46.62.214.182/ready` answers (Caddy up; certificate not yet valid — that's expected until DNS).

1. Delete the proxied CNAMEs `llunde.no` and `www.llunde.no` → tunnel `ed8abcdb-...cfargotunnel.com`.
2. Add **grey-cloud (DNS only)**: `llunde.no` A `46.62.214.182`, AAAA `2a01:4f9:c014:cbe0::1`; `www` CNAME `llunde.no`; `api` A + AAAA same values.
3. Wait for propagation (`dig +short llunde.no api.llunde.no`), let Caddy obtain Let's Encrypt certs, then run §8.
4. Only after §8 passes: Zero Trust → Networks → Tunnels → delete the **llunde** tunnel (`ed8abcdb-508c-4d61-85b7-bc3127510e4b`). Do not touch the pyparser tunnel.

## 8. Gate verification (phase-2 README, as commands)

```sh
curl -s https://api.llunde.no/ready                        # {"database":true,"valkey":true}
curl -s -o /dev/null -w '%{http_code}\n' https://llunde.no # 200 (frontend)
for p in metrics health ready; do curl -s -o /dev/null -w "$p %{http_code}\n" https://api.llunde.no/$p; done
                                                           # blocked publicly (403/404) — except /ready if deliberately allowed: expect per stream C2's Caddyfile
curl -s http://<TAILNET_IP>:<NODE_EXPORTER_PORT>/metrics | head -1   # reachable over tailnet only
# auth lifecycle (backend gate, against prod):
EMAIL="gate-$(date +%s)@example.com"
curl -s -X POST https://api.llunde.no/auth/register -H 'Origin: https://llunde.no' -H 'X-CSRF-Token: t' \
  -H 'Content-Type: application/json' -d "{\"email\":\"$EMAIL\",\"password\":\"correct horse battery\"}"   # 201
# ...login/me/sessions/logout-all as in the backend runthrough; verify audit log shows YOUR real IP, not a proxy IP
# isolation:
ssh root@<TAILNET_IP> "sudo -u llunde-frontend curl -s --max-time 3 http://llunde-postgres:5432 || echo ISOLATED"
# auto-update: push a trivial backend image change to main, wait for the timer (or poke:
ssh root@<TAILNET_IP> systemctl --user -M llunde-backend@ start podman-auto-update.service
# backups: force one run, list snapshots, restore one dump into a scratch db (stream D2's unit names):
ssh root@<TAILNET_IP> systemctl start restic-backups-llunde-backend.service   # upstream restic module naming, confirmed
# reboot test:
ssh root@<TAILNET_IP> reboot && sleep 90 && curl -s https://api.llunde.no/ready
```

## 9. Update & rollback

- **App images**: pull-based — per-user `podman-auto-update.timer` polls GHCR `:latest` (`AutoUpdate=registry`). Manual poke: §8's `systemctl --user -M <user>@ start podman-auto-update.service`. Pin/rollback an image: set the unit's `Image=` to a digest in `services/<name>/default.nix`, deploy (§6).
- **Config**: `nixos-rebuild --rollback switch` on the host, or pick the previous generation in the systemd-boot menu; every deploy is a new generation.

## 10. Restore from backup (rehearse once at the gate)

```sh
export RESTIC_REPOSITORY="s3:s3.eu-north-1.amazonaws.com/llunde-pyparser-bucket/<RESTIC_PREFIX>"   # from modules/backups
restic snapshots            # password + AWS env from secrets/restic.yaml values
restic restore latest --target /tmp/restore
# postgres (dump file path per stream D2's preHook):
ssh root@<TAILNET_IP> "podman exec -i -u postgres llunde-postgres psql -U llunde llunde" < /tmp/restore/<DUMP_PATH>
# valkey: stop unit, replace appendonly dir from restore, start unit
```

## 11. Break-glass SSH & port-22 posture (ADR 015)

**Public port 22 is closed on both boxes** (phase 3). Every SSH consumer rides the tailnet: laptop (`ssh root@llunde-01` / `root@llunde-parser.tail0b6cbe.ts.net`), pyparser CI, portfolio CI (each joins per-run with an ephemeral `tag:ci` key).

**Break-glass, per host** (tailnet down or node expired):

1. Hetzner Cloud console (web VNC) — always works, no network path needed. `hcloud server request-console <name>` or the dashboard. Root password login is disabled; use the console for single-user/rescue boot, or:
2. `hcloud server enable-rescue <name> && hcloud server reset <name>` — boots the rescue system with your Hetzner SSH key on public 22 (rescue ignores the cloud firewall's intent by being a different boot target — still gated by the firewall, so pair with step 3).
3. Re-open 22 temporarily: add the `port = "22"` rule back in `tofu/llunde/firewall.tf` (llunde-01) or `tofu/pyparser/server.tf`'s firewall (llunde-parser), `tofu apply`. **llunde-parser only**: also `ufw allow 22` once you're in. Revert both when done — the closed state is the committed one.

llunde-01's NixOS host firewall never listed 22 for the public interface after closure (`modules/profiles/server.nix`); `tailscale0` is a trusted interface, so sshd stays reachable over the tailnet regardless.
