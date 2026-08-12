# Runbook — bare Hetzner server → serving llunde

Executed literally at the phase-2 go-live and re-executed on any reprovision.
**Every deviation discovered during execution is a documentation bug — fix it here, on the spot** ([phase-2 tasks 2.4](init/plans/phase-2/tasks.md)).

As of phase 3.5 the estate is **two hosts**. §§1–10 are written against
`llunde-01` and stay as executed; §11 already covers both; **§12 lists the
per-host deltas for `llunde-parser`** — read it alongside the matching section
before running anything there.

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
5. GitHub CLI auth (hardened 2026-08-12): `gh` runs a **fine-grained PAT** scoped to the
   five llunde repos — Contents RW, Pull requests RW, Actions read, Metadata — with
   **deliberately NO Administration** (deploy keys) **and NO Workflows** permissions.
   Repo pushes ride SSH (`git@github.com`), which sidesteps the workflow-file push
   restriction; PR merges of workflow changes work without it (proven). If a task needs
   deploy-key or workflow-file API writes, mint a *transient* token — don't broaden this
   one. ⏰ **90-day expiry: renewal due ~2026-11-10** (then every 90 days).

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
doppler run --project pyparser --config prd -- tofu -chdir=tofu apply
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

## 6. Deploy configuration changes (steady-state loop — PULL, ADR 020)

**Merging to `main` IS the deploy.** The gate (`Check`) passes → `promote.yml`
fast-forwards `deploy` → each host applies itself (`modules/gitops-pull`):

- **llunde-01** within ~5 min of promotion (the canary)
- **llunde-parser** +30 min behind (refuses younger revs)

```sh
ssh root@<TAILNET_IP> journalctl -fu gitops-pull      # watch a rollout
ssh root@<TAILNET_IP> cat /var/lib/gitops-pull/applied  # which rev is live
```
(also `gitops_pull_applied_info` / `gitops_pull_last_success_timestamp` in Prometheus)

The pull loop **subsumes the old post-switch restart step** (phase-4 review
B4): units whose quadlet file or bind-mounted config changed (Caddyfile,
cloudflared, prometheus.yml, …) are restarted by the module's reconcile map
(`llunde.gitopsPull.reconcile` in each host file). **A NEW quadlet unit or
bind-mounted config MUST be added to that map** — otherwise its config changes
land on disk but never go live: NixOS's switch only daemon-reloads user
managers, and podman resolves bind mounts at container *creation*.

Manual deploy (the recovery route — must always work, unchanged mechanics):

```sh
ssh root@<TAILNET_IP> systemctl stop gitops-pull.timer   # pause the loop FIRST
nix run nixpkgs#nixos-rebuild -- switch --flake .#<host> \
  --target-host root@<TAILNET_IP> --build-host root@<TAILNET_IP>
# ...restart affected user units per the reconcile map, then:
ssh root@<TAILNET_IP> systemctl start gitops-pull.timer
```

⚠️ **A rev that changes BOTH a reconcile `check` command AND the config that
check validates will be graded by the OLD check.** gitops-pull runs from the
RUNNING generation (`restartIfChanged = false`, the self-deploying-deployer
guard), so a new check string only takes effect on the NEXT run. H1b hit this:
its `acme_dns` Caddyfile was validated by the previous `pkgs.caddy` check, which
lacked the DNS provider, so the restart was skipped and the deploy stalled with
a sticky `reconcile_failed` while the old container kept serving. **Land a check
change in its own rev first** — it is a no-op against the old config — then the
config change. Recovery if it bites: run the new check by hand, restart the unit,
`rm /var/lib/gitops-pull/reconcile_failed`, `systemctl start gitops-pull`.

⚠️ The loop converges to `deploy`: a manually deployed rev that is NOT the
`deploy` tip gets rolled back to it on the next cycle. Keep the timer stopped
until your change is merged **and promoted** (this bit during the 2c
bootstrap: the enable-commit's own promotion raced the first poll).

## 7. Edge cutover — llunde.no onto the tunnel, then zero public inbound (phase-4 E5/B5/E6, ADR 017)

> Direction matters. This section used to describe the **phase-2** cutover, which
> moved llunde.no *off* the retired `ed8abcdb` tunnel onto direct A/AAAA records.
> That tunnel is gone. This is the **phase-4** cutover, which moves llunde.no
> *onto* the new `c0cdd9b5` tunnel and then closes 80/443 — the estate's last
> public inbound ports. Zone `llunde.no` = `4ae54b24fc4140d4d1c450491645f1c8`.

Tofu is **manual and owner-run** — it is deliberately not on the pull loop (§6).
Env for every `tofu` call in this section:

```sh
eval "$(aws configure export-credentials --format env)"          # S3 state; session expires — re-auth is yours
export TF_VAR_hcloud_token=$(awk -F'"' '/^[[:space:]]*token/ {print $2; exit}' ~/.config/hcloud/cli.toml)
export CLOUDFLARE_API_TOKEN=$(doppler secrets get CLOUDFLARE_API_TOKEN --project llunde --config ops --plain)
tofu -chdir=tofu plan       # ALWAYS read the plan before apply
```

Stale S3 lock (a killed apply): `tofu force-unlock <id>` — only after confirming
nothing is actually running.

**Preconditions** (all four, verified before step 1):

- H1 is live: Caddy issues via **DNS-01** (§13) — certs stay warm with 80/443
  closed, so rollback never waits on Let's Encrypt.
- The tunnel's ingress map covers all three hostnames (§13 reads it out of the
  connector's own log — the dashboard is not the only place to look).
- `blackbox-cfray` is deployed on llunde-parser and **RED**. That is correct
  before the flip: it asserts CF-Ray on `https://llunde.no`, which direct A
  records cannot produce. It turning green IS the cutover's success signal.
- The pull loop stays **RUNNING** throughout. E5 and B5 touch no repo file, E6
  must ride the loop to be declarative, and both rollback levers (`git revert` +
  merge, `git push -f <sha>:deploy`) need it running. Just don't merge anything
  unrelated during the window.

### 7.1 E5a — drop the AAAA records (tofu apply #1)

A CNAME cannot coexist with an A **or** AAAA record at the same name, and tofu
gives **no ordering guarantee** between an unrelated create and destroy in the
same apply. Written as one apply, E5 can die on Cloudflare error 81053 ("An A,
AAAA, or CNAME record with that host already exists") — and *may* pass once and
fail on a re-run. So the v6 records go first, on their own.

```sh
tofu -chdir=tofu apply                       # plan: 3 to destroy (aaaa: llunde.no, www, api)
for h in llunde.no www.llunde.no api.llunde.no; do dig +short AAAA "$h"; done   # all empty
curl -s -o /dev/null -w '%{http_code}\n' https://llunde.no    # 200, still direct over v4
```

IPv6-only clients lose the site between 7.1 and 7.2 — minutes. CF restores v6 at
the edge in 7.2, so v6 reachability is net *better* afterwards.

**Rollback**: `git revert` the PR, apply. Records return.

### 7.2 E5b — A → proxied CNAME (tofu apply #2) 💥 THE FLIP

The `moved {}` block keeps the same resource address, so tofu updates the
existing record ids in place instead of create-before-destroy.

```sh
tofu -chdir=tofu apply     # plan: 3 to change (A 46.62.214.182 -> CNAME <tunnel>.cfargotunnel.com, proxied)
```

Proxied records propagate in seconds — CF answers authoritatively and the TTL is
already `auto` (300 s). Budget **≤5 min** for resolver caches; anything still
resolving `46.62.214.182` after that is a stale local resolver, not a failure.

Verify, in order — **stop and roll back on the first failure**:

```sh
# 1. the records moved
dig +short llunde.no www.llunde.no api.llunde.no      # CF anycast (104.21.x/172.67.x), NOT 46.62.214.182

# 2. traffic rides the CF edge (this is what blackbox-cfray asserts continuously)
for h in llunde.no www.llunde.no api.llunde.no; do
  printf '%-16s %s\n' "$h" "$(curl -sS -o /dev/null -D - "https://$h" | grep -ci '^cf-ray:')"
done                                                   # each -> 1

# 3. www -> apex redirect survives the tunnel
curl -sS -o /dev/null -w '%{http_code} %{redirect_url}\n' https://www.llunde.no/x   # 301 https://llunde.no/x

# 4. ops endpoints 403 THROUGH THE EDGE — the variants, not just the bare paths.
#    A 404 means the request reached the backend; only a 403 proves Caddy blocked it.
for p in metrics metrics/ metrics// metrics%2f METRICS metrics/x health health/ ready ready/; do
  printf '%-12s %s\n' "$p" "$(curl -sS --path-as-is -o /dev/null -w '%{http_code}' "https://api.llunde.no/$p")"
done                                                   # every one 403

# 5. the real-IP contract, end to end (ADR 017's non-optional verification)
EMAIL="cutover-$(date +%s)@example.com"
curl -sS -i -X POST https://api.llunde.no/auth/register -H 'Origin: https://llunde.no' \
  -H 'X-CSRF-Token: t' -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"correct horse battery\"}"
#    -> 201, and Set-Cookie carries `Secure` (proves X-Forwarded-Proto=https survives the plain-HTTP hop)
#    -> the backend audit row for this registration shows YOUR public IP, not 127.0.0.1 and not a CF address
#    -> forged headers are discarded: repeat with -H 'X-Forwarded-For: 9.9.9.9' and the audit IP is unchanged

# 6. the collectors agree
curl -s http://100.92.219.50:9090/api/v1/targets | jq -r '.data.activeTargets[] | "\(.labels.job) \(.health)"' | sort
#    blackbox-cfray -> up AND probe_success 1 (it was 0 before the flip)
```

**Rollback** (any failure above): `git revert` the 7.2 PR, `tofu apply`. The A
records return, Caddy serves them with certs that are still valid (H1), and
80/443 are still open because B5 has not run yet. Total: one apply + propagation.

### 7.3 B5 — close 80/443 at the Hetzner cloud firewall (NOT tofu)

⚠️ **The hcloud provider cannot delete a firewall's last rules.** `llunde-fw` has
exactly two (80, 443); an apply that removes both omits the rules field entirely,
silently no-ops, and prints "Apply complete" — a lie. Same trap documented in
`tofu/parser-server.tf`, where going to zero was also done out-of-band.

```sh
hcloud firewall describe llunde-fw                       # confirm: exactly the 80 + 443 rules
echo '[]' | hcloud firewall replace-rules --rules-file - llunde-fw
hcloud firewall describe llunde-fw                       # Rules: (empty)

# instantly verify from OFF the tailnet — the origin must stop answering
curl -sS --max-time 8 http://46.62.214.182/  ; echo "exit=$? (expect 28 = timeout)"
curl -sS --max-time 8 https://46.62.214.182/ ; echo "exit=$? (expect 28 = timeout)"
curl -s -o /dev/null -w '%{http_code}\n' https://llunde.no    # 200 — the tunnel path is unaffected
```

⚠️ **`--rules-file` takes `-` for stdin — use it.** The originally recorded
`--rules-file <([])` is a typo: `[]` is not a command, so the process
substitution hands hcloud an **empty file** while the shell still exits 0.
`<(echo '[]')` works, but piping to `-` is unambiguous across shells and has no
process-substitution trap at all. Executed that way at the 2026-08-13 cutover.

Then reconcile tofu so the next plan is clean (the rules are gone from the API but
still in state):

```sh
tofu -chdir=tofu apply -refresh-only     # state learns the firewall has no rules
tofu -chdir=tofu plan                    # after the E6 PR lands the HCL: "No changes."
```

🚧 **Between 7.3 and the 7.4 merge, do not run a plain `tofu apply`.** The HCL
still declares the 80/443 rules, so an apply would re-open them at the cloud edge
— quietly undoing the step you just took. `-refresh-only` is safe; a full apply
is not, until 7.4's PR has removed the rules from `tofu/llunde-firewall.tf`.

**Rollback** (seconds, and it works with the tailnet down — the firewall name is
positional and goes last):

```sh
hcloud firewall add-rule --direction in --protocol tcp --port 80  --source-ips 0.0.0.0/0,::/0 llunde-fw
hcloud firewall add-rule --direction in --protocol tcp --port 443 --source-ips 0.0.0.0/0,::/0 llunde-fw
```

### 7.4 E6 — close 80/443 in NixOS (rides the pull loop)

The PR removes `80`/`443` from `llunde.profile.publicTCPPorts` in
`hosts/llunde-01/default.nix` and drops the two `rule` blocks from
`tofu/llunde-firewall.tf` (matching what 7.3 already did to the API). Merge →
gate → promote → llunde-01 self-applies within ~5 min.

**This cannot trigger the deadman.** Its probes are management-plane only —
tailnet online (or peer ping), `sshd` listening on 22, `git ls-remote` fetchable —
and none of them traverses 80/443. `tailscale0` stays a trusted interface, so SSH
is unaffected. Verified against `modules/gitops-pull/default.nix`, not assumed.

```sh
ssh root@100.109.80.121 journalctl -fu gitops-pull        # watch it land
ssh root@100.109.80.121 cat /var/lib/gitops-pull/applied  # == the merged sha
# NixOS's firewall here is IPTABLES, not nftables (`nft list ruleset` is empty on
# these hosts) — the chain is nixos-fw, and both families must be checked:
ssh root@100.109.80.121 'iptables -S nixos-fw | grep -E "dport (80|443)"'   # no output
ssh root@100.109.80.121 'ip6tables -S nixos-fw | grep -E "dport (80|443)"'  # no output
ssh root@100.109.80.121 'ss -ltn | grep -E ":(80|443) "'  # Caddy STILL BINDS them — the firewall is what closed
curl -s -o /dev/null -w '%{http_code}\n' https://llunde.no                                # 200
ssh root@100.92.219.50 cat /var/lib/gitops-pull/applied   # parser follows +30 min; heartbeats green on both
```

**Rollback**: `git revert` + merge — auto-applies in ~5 min. If you need it faster
or the gate is red: `git push -f <good-sha>:deploy`. Neither restores the *cloud*
firewall — 7.3's `hcloud firewall add-rule` is the lever for that, and it is the
one that actually matters.

### 7.5 Full break-glass (tunnel or Cloudflare itself is broken)

Certs are warm (H1/DNS-01, renewed independently of any open port), so this is
DNS + firewall only, in this order:

1. `hcloud firewall add-rule` ×2 (7.3's rollback) — the origin answers again.
2. `git revert` the E6 PR + merge — the NixOS firewall follows within ~5 min.
   (Steps 1 and 2 are both needed; either layer alone still blocks.)
3. `git revert` the 7.2 PR + `tofu apply` — A records return, grey-cloud.
4. Optionally revert 7.1 to restore AAAA.

Neither Caddy nor the CF edge sets HSTS (verified 2026-08-12), so browsers can
click through any interim TLS error — the window is degraded, not hard-failed.
Step 3 requires the Cloudflare **DNS API** to work; it does not require the CF
proxy or tunnel to work, which is exactly the outage this path is for.

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
- **Config — roll back the estate**: `git push -f <good-sha>:deploy` from the laptop. Works with the tailnet down (`deploy` is deliberately unprotected so this lever always exists); hosts converge within a cycle (llunde-01) / after the 30-min lag (parser). Then fix forward on main via PR — the next green merge fast-forwards `deploy` back onto main's history.
- **Pause all applies**: GitHub → Actions → *Promote to deploy* → Disable workflow (freezes `deploy`, both hosts hold). Per host: `systemctl stop gitops-pull.timer`.
- **Gate wedged by a GitHub outage, deploys blocked** (precedent 2026-08-12): the
  `tofu` job failed twice fetching provider `SHA256SUMS` from GitHub release
  assets (503, then a timeout) while GitHub's status page read "Actions: Normal"
  — release-asset delivery is a different component. Under pull auto-apply a red
  gate means NO deploys, so this freezes the estate. `deploy` was advanced by
  hand: `git push origin <sha>:deploy`. **Only with evidence the gate is lying**
  — there, main's TREE was byte-identical to a PR head whose Check was fully
  green, the diff touched no tofu at all, and `nix flake check` + `tofu fmt` +
  `tofu validate` were re-run locally on the merged commit first. This BYPASSES
  ADR 020's gate-green invariant; it is a break-glass lever, not a shortcut. The
  provider cache added to the gate afterwards makes the failure mode much less
  likely.
- **After a deadman rollback** (host rebooted into the previous generation; you got the healthchecks `/fail` email): the offending rev is **HELD** on that host — no reboot loop. Fix forward on main; the newly promoted rev clears the hold automatically. To force-retry the same rev instead: `rm /var/lib/gitops-pull/attempting` on the host.
- **Sticky reconcile failure** (`/var/lib/gitops-pull/reconcile_failed` exists, heartbeat failing while "converged"): a unit restart failed after a switch. Fix the unit; any successful apply clears the marker (or remove it by hand).
- **Host unreachable and the deadman didn't fire**: `hcloud server reset <name>` — boots the previous boot-default generation (the loop never moves the boot default before its probes pass). Rehearsed 2026-08-11.
- **Host won't BOOT** (kernel/initrd/bootloader — the accepted boot-plane residual, ADR 020): Hetzner console (web VNC) → systemd-boot menu → select the previous generation.
- **Emergency local rollback** (on a reachable host): `nixos-rebuild --rollback switch` still works — but remember the loop will re-converge to `deploy` unless you stop the timer.

## 10. Restore from backup (rehearse once at the gate)

```sh
export RESTIC_REPOSITORY="s3:s3.eu-north-1.amazonaws.com/llunde-pyparser-bucket/<RESTIC_PREFIX>"   # from modules/backups
restic snapshots            # password + AWS env from secrets/restic.yaml values
restic restore latest --target /tmp/restore
# postgres (dump file path per stream D2's preHook):
ssh root@<TAILNET_IP> "podman exec -i -u postgres llunde-postgres psql -U llunde llunde" < /tmp/restore/<DUMP_PATH>
# valkey: stop unit, replace appendonly dir from restore, start unit
```

## 11. Break-glass SSH & public-port posture (ADR 015 + ADR 017)

**The estate has ZERO public inbound ports.** Port 22 closed in phase 3; llunde-01's 80/443 closed at the phase-4 E6 cutover (2026-08-13). Every SSH consumer rides the tailnet: laptop (`ssh root@llunde-01` / `root@llunde-parser.tail0b6cbe.ts.net`), pyparser CI, portfolio CI (each joins per-run with an ephemeral `tag:ci` key). All web ingress arrives through Cloudflare tunnels the hosts dial outbound.

**Re-opening 80/443** (the web half of break-glass — see §7.5 for the full sequence):

```sh
hcloud firewall add-rule --direction in --protocol tcp --port 80  --source-ips 0.0.0.0/0,::/0 llunde-fw
hcloud firewall add-rule --direction in --protocol tcp --port 443 --source-ips 0.0.0.0/0,::/0 llunde-fw
```

Caddy never stopped binding those ports, so the origin serves the moment the rule lands — and its certificates are valid, because renewal runs over DNS-01 and never needed an inbound port. Restoring the NixOS half (`publicTCPPorts`) is a `git revert` of the E6 commit; both layers must be open for public traffic to arrive.

**Break-glass, per host** (tailnet down or node expired):

1. Hetzner Cloud console (web VNC) — always works, no network path needed. `hcloud server request-console <name>` or the dashboard. Root password login is disabled; use the console for single-user/rescue boot, or:
2. `hcloud server enable-rescue <name> && hcloud server reset <name>` — boots the rescue system with your Hetzner SSH key on public 22 (rescue ignores the cloud firewall's intent by being a different boot target — still gated by the firewall, so pair with step 3).
3. Re-open 22 temporarily: add the `port = "22"` rule back in `tofu/llunde-firewall.tf` (llunde-01) or `tofu/parser-server.tf` (llunde-parser), `tofu apply`. **llunde-parser only**: also `ufw allow 22` once you're in. Revert both when done — the closed state is the committed one.

llunde-01's NixOS host firewall never listed 22 for the public interface after closure (`modules/profiles/server.nix`); `tailscale0` is a trusted interface, so sshd stays reachable over the tailnet regardless.

## 12. llunde-parser (phase 3.5) — per-host deltas

Same runbook, second host. Only the differences from §§2–10 are listed; the
full cutover choreography (freeze, rehearsal, restore order, gate metrics)
lives in [phase-3.5 tasks](init/plans/phase-3.5/tasks.md) — this section is the
reusable reprovision knowledge.

- **§2 Provision — nothing to do.** The server exists (Hetzner 141119325,
  CCX23, adopted in tofu during phase 3); no tofu changes are needed for the
  reinstall and `tofu plan` must stay "No changes." throughout — the wipe is
  invisible to the cloud API.
- **§3 Identity & secrets**: host key pre-generated at
  `/tmp/llunde-parser-keys/etc/ssh/ssh_host_ed25519_key` (phase-3.5 step 0.2 —
  done *before* the rehearsal, not at cutover, because the rehearsal box
  decrypts with the same key). Secret files per the phase-3.5 contract:
  `secrets/pyparser-doppler.yaml` + `secrets/pyparser-restic.yaml` (new),
  `secrets/tailscale.yaml` + `secrets/ghcr.yaml` (shared — `sops updatekeys`
  after adding the host recipient to `.sops.yaml`'s per-host creation rules).
  No DB-password sops file: `POSTGRES_PASSWORD` is single-sourced from the
  Doppler `pyparser/prd` render.
- **§4 Install** (💥 destroys the box — both tenants down for the window):
  ```sh
  nix run github:nix-community/nixos-anywhere -- \
    --flake .#llunde-parser \
    --build-on-remote \
    -i ~/.ssh/id_ed25519 \
    --extra-files /tmp/llunde-parser-keys \
    root@<LLUNDE_PARSER_IP>
  ```
  ⚠️ **CRITICAL, before the new install's `tailscale up`** (i.e. immediately
  after the wipe starts, [tasks 3.3](init/plans/phase-3.5/tasks.md)): free the
  `llunde-parser` MagicDNS name. **As executed (no console needed)**: from the
  old box itself, `tailscale set --hostname llunde-parser-old` — the record
  renames live, the name frees instantly (verify: the old FQDN stops
  resolving), and the corpse record can be deleted from the console at
  leisure. Skip this and the new box joins as `llunde-parser-1` while every
  consumer (portfolio's `DEPLOY_HOST`, ssh aliases) resolves a corpse.

  ⚠️ **The installer needs public 22** — after kexec the box runs the
  nixos-anywhere installer, which has no tailscaled: an install over the
  tailnet IP dies mid-flight. As executed: temporarily
  `hcloud firewall add-rule` 22 (+ `ufw allow 22` while the old OS is still
  Ubuntu), install against the **public** IP, then
  `hcloud firewall replace-rules … <([])` back to zero-inbound once the new
  system is up (the NixOS host firewall opens nothing regardless).
- **§5 Verify**: same checks with hostname `llunde-parser` (four secrets in
  `/run/secrets/`, tailscale joined as **exactly** `llunde-parser`). Extra,
  per the zero-public-ports posture: `ss -tlnp` on the box shows no public
  listeners at all — not even 80/443 (ingress is the Cloudflare tunnels,
  outbound). And both tenants exist:
  `systemctl --user -M pyparser@ list-units 'pyparser-*'` for the stack;
  `id portfolio` (uid 3000) + `grep portfolio /etc/subuid` (100000:65536) +
  linger for the slot.
- **§6 Deploy loop**: identical, with `.#llunde-parser` and this host's
  tailnet address as `--target-host`/`--build-host`. Quadlet check:
  `systemctl --user -M pyparser@ list-units 'pyparser-*'`.
- **§7 does not apply** — llunde-parser never had a DNS cutover; both its
  hostnames were already on the pyparser tunnel, and it has had zero public
  inbound since phase 3. **§13 does apply**, with the pyparser tunnel id
  (`e77d6ebf-…`), uid 2001, and the `:latest`/shared-network connector shape —
  read the table there for what differs. **§8**'s gate metrics here are
  `parser.llunde.no`, `external.llunde.no`, `hansteen.dev`.
- **§9 Update & rollback**: same pull-based model; the pyparser digest-pin
  lever lives in `services/pyparser/`. Details in
  [docs/pyparser/PROD.md](pyparser/PROD.md).
- **§10 Restore**: `<RESTIC_PREFIX>` = `restic/llunde-parser`. pyparser only —
  `restic restore latest --target /tmp/restore`, then the custom-format dump
  goes through `pg_restore` (not psql):
  ```sh
  runuser -u pyparser -- env XDG_RUNTIME_DIR=/run/user/2001 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2001/bus \
    podman exec -i pyparser-postgres pg_restore --clean --if-exists \
    -U pyparser -d pyparser_llunde < /tmp/restore/var/backup/pyparser/pyparser_llunde.dump
  # files: copy the restored pyparser-files/_data contents back into the volume
  ```
  **portfolio restores itself** from its own S3 backups per its own DR
  runbooks — llunde-infra hands off at the slot boundary (ADR 016).

## 13. cloudflared & the tunnel edge — ops (ADR 017)

The llunde tunnel is **`c0cdd9b5-fa97-42a1-bca7-95da236ea949`**, terminated by the
`cloudflared` quadlet under `edge` (uid 2000) on llunde-01: `Network=host`,
**digest-pinned**, token from `/run/secrets/llunde-tunnel.env`. It dials Caddy's
plain-HTTP `:8085` loopback listener; Cloudflare terminates public TLS.
Do **not** confuse it with pyparser's tunnel `e77d6ebf-…` (parser/external
hostnames, llunde-parser) — that one is not ours to touch.

⚠️ **Never rotate the tunnel token casually.** A tunnel's token embeds its
secret; regenerating it drops the live connectors — i.e. the front door — until
the new token is deployed through sops + a switch.

### 13.1 Is the edge healthy?

```sh
# connectors (expect 4, one per CF colo the daemon picked)
cd / && ssh root@100.109.80.121 'cd / && runuser -u edge -- env XDG_RUNTIME_DIR=/run/user/2000 \
  DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2000/bus \
  journalctl --user -u cloudflared -n 200 --no-pager | grep -c "Registered tunnel connection"'

# the container itself (NB: quadlet names it systemd-caddy / llunde-cloudflared)
ssh root@100.109.80.121 'cd / && runuser -u edge -- env XDG_RUNTIME_DIR=/run/user/2000 \
  DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2000/bus podman ps --format "{{.Names}} | {{.Image}}"'

# from outside: CF-Ray present == the request rode the edge
curl -sS -o /dev/null -D - https://llunde.no | grep -i '^cf-ray:'
```

`runuser` needs a cwd the target user can read — hence the `cd /`. Without it you
get `cannot chdir to /root: Permission denied`, which looks like a podman failure
and is not.

### 13.2 What ingress map is the tunnel actually serving?

The hostname→service map is **remote** (Cloudflare-side) config that the connector
fetches at startup and on change. The connector's own log is the ground truth for
what it is running right now:

```sh
ssh root@100.109.80.121 'cd / && runuser -u edge -- env XDG_RUNTIME_DIR=/run/user/2000 \
  DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2000/bus \
  journalctl --user -u cloudflared --no-pager | grep "Updated to new configuration" | tail -1'
```

The contract (all three vhosts land on the one Caddy listener, which routes by
`Host`):

```json
{"ingress":[{"hostname":"llunde.no","service":"http://localhost:8085"},
            {"hostname":"www.llunde.no","service":"http://localhost:8085"},
            {"hostname":"api.llunde.no","service":"http://localhost:8085"},
            {"service":"http_status:404"}],
 "warp-routing":{"enabled":false}}
```

Cloudflare-side view (needs the ops token's `Account:Cloudflare Tunnel:Read`):

```sh
export CLOUDFLARE_API_TOKEN=$(doppler secrets get CLOUDFLARE_API_TOKEN --project llunde --config ops --plain)
curl -s -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" \
  "https://api.cloudflare.com/client/v4/accounts/8786559b30fcebd08d0c594b6e899eef/cfd_tunnel/c0cdd9b5-fa97-42a1-bca7-95da236ea949/configurations" \
  | jq '.result.config.ingress'
```

A `401`/`10000 Authentication error` here means you are holding the **old
DNS-only** ops token — not that the tunnel is broken.

### 13.3 Changing the connector

The ingress map is tofu-managed (`tofu/modules/cloudflare/`) — a dashboard edit
is drift and will be reverted by the next apply. Config changes reach the running
connector within seconds without a restart (it re-fetches; you'll see a new
`Updated to new configuration … version=N` line).

The **unit** (image digest, memory) is NixOS-declared: edit `modules/ingress/`,
regenerate `tests/golden/cloudflared.container`, PR → merge → the pull loop
restarts it via the reconcile map (`hosts/llunde-01/default.nix`). The front door
never auto-updates: `autoUpdate = false` for `edge` is deliberate, and image
bumps are deliberate digest edits.

Hand restart (only off the loop — stop `gitops-pull.timer` first, §6):

```sh
ssh root@100.109.80.121 'cd / && runuser -u edge -- env XDG_RUNTIME_DIR=/run/user/2000 \
  DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2000/bus systemctl --user restart cloudflared'
```

### 13.4 Failure modes, in the order they actually happen

| Symptom | Cause | Move |
|---|---|---|
| Site 502/1033, connectors 0 | connector down or token invalid | `systemctl --user status cloudflared`; check `/run/secrets/llunde-tunnel.env` materialized |
| Site 404 (empty body) on every hostname | Caddy's `:8085` site address grew a host — `http://127.0.0.1:8085` makes `127.0.0.1` the **Host matcher** and nothing matches | address must be hostless `http://:8085` + `bind 127.0.0.1` (goldened; the golden is the guard) |
| One hostname 404s, others fine | that hostname missing from the ingress map | §13.2, then fix in tofu |
| `400` on everything through the tunnel | M1 guard: no `CF-Connecting-IP` — the request did not come from CF | expected for a direct hand-probe of `:8085`; add `-H 'CF-Connecting-IP: 1.2.3.4'` to test |
| Rate limits key on one bucket / audit shows `127.0.0.1` | the XFF←`CF-Connecting-IP` mapping was lost | `tests/golden/llunde-01.Caddyfile` diff; it cannot regress silently |
| Cloudflare itself is down | — | §7.5 full break-glass |
