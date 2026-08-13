# Runbook — operating the llunde estate

Two NixOS hosts, `llunde-01` and `llunde-parser`, fully declarative, **zero public inbound ports**.
Web ingress arrives through Cloudflare Tunnels the hosts dial *outbound*; SSH rides Tailscale; no
record in the `llunde.no` zone resolves to a host address. Observability
(Prometheus/Grafana/Loki/OTel/blackbox) runs on `llunde-parser`, tailnet-only, with an external
dead-man. Deploys are **pull** (§6) — no CI ever SSHes a host.

- **§§1–5** — rebuilding a host. Order is load-bearing: secrets must exist *before* install,
  because the host key is pre-generated and injected.
- **§6, §§8–10** — steady state: deploy, verify, roll back, restore.
- **§7, §11, §13** — the edge: DNS, firewalls, the tunnel, every break-glass lever.
- **§12** — `llunde-parser` deltas; read it alongside the matching section (§§1–11 are written
  against `llunde-01`) before running anything on the parser.

`<ANGLE_BRACKETS>` are fill-ins; each states its source. **Any deviation you hit is a
documentation bug — fix it here, on the spot.**

---

## 1. Laptop prerequisites

**Nix** is installed natively (`nix 2.35.1` via `~/.nix-profile`) with `nixd`, `statix`,
`alejandra`, `deadnix`. **Tools**: `opentofu`, `sops`, `age`, `awscli` via brew; `ssh-to-age`
has no brew formula — `nix run nixpkgs#ssh-to-age` (or `nix profile install nixpkgs#ssh-to-age`);
`hcloud` authenticated (`hcloud server list` shows 132168416); `doppler` logged in.

**Credentials — source, and what expires:**

- **Owner age key** — `~/.config/sops/age/keys.txt`, public key
  `age13upkqrd7v97a4gcgerwnkuwvznxh60u2g56x6cj55hemwq68q3lsalynwn`, the admin recipient in
  `.sops.yaml`. **Back it up** — without it nothing in `secrets/` decrypts.
- **Hetzner token** for tofu, from the hcloud CLI config:
  ```sh
  export TF_VAR_hcloud_token=$(awk -F'"' '/^[[:space:]]*token/ {print $2; exit}' ~/.config/hcloud/cli.toml)
  ```
- **AWS** (S3 tofu state) — from Doppler on every tofu call: prefix with
  `doppler run --project pyparser --config prd --`. An admin AWS profile also exists locally via
  `aws login` (root credentials); its session expires and re-auth is yours.
- **Cloudflare ops token** —
  `doppler secrets get CLOUDFLARE_API_TOKEN --project llunde --config ops --plain`; needs
  `Account:Cloudflare Tunnel:Read` for §13.2.
- **GitHub CLI** — a **fine-grained PAT** over the five llunde repos: Contents RW, Pull requests
  RW, Actions read, Metadata, with **deliberately NO Administration** (deploy keys) and **NO
  Workflows**. Repo pushes ride SSH (`git@github.com`), sidestepping the workflow-file push
  restriction; PR merges of workflow changes work without it. Need deploy-key or workflow-file
  API writes? Mint a *transient* token — don't broaden this one.
  ⏰ **90-day expiry: renewal due ~2026-11-10**, then every 90 days.

The other clock: Caddy's origin certificates renew over DNS-01 at ~30 days remaining, alert at
21 (§13.5).

### Module test bench: the `nixos-dev` VM

`ssh nixos-dev` — ProxyJump via the Arch desktop `archie` over Tailscale; 4 vCPU/6 GiB, NixOS
26.05 minimal, libvirt NAT. Build this flake's closure there (`nixos-rebuild build --flake
.#llunde-01` after rsyncing the repo) or dry-activate modules. Hcloud specifics (disko device
names, firewall) and the flake's 25.11 pin vs the VM's 26.05 differ — test bench, not replica.

## 2. Provision (tofu)

Both servers exist in tofu state: `llunde-01` (Hetzner 132168416), `llunde-parser` (141119325,
CCX23). A wipe-and-reinstall is invisible to the cloud API — a rebuild needs no tofu change and
`tofu plan` must stay "No changes."

Tofu is **manual and owner-run**, deliberately off the pull loop. Env for every `tofu` call in
this runbook:

```sh
eval "$(aws configure export-credentials --format env)"   # S3 state; session expires — re-auth is yours
export TF_VAR_hcloud_token=$(awk -F'"' '/^[[:space:]]*token/ {print $2; exit}' ~/.config/hcloud/cli.toml)
export CLOUDFLARE_API_TOKEN=$(doppler secrets get CLOUDFLARE_API_TOKEN --project llunde --config ops --plain)
tofu -chdir=tofu plan       # ALWAYS read the plan before apply
```

Or take the AWS half straight from Doppler:

```sh
doppler run --project pyparser --config prd -- tofu -chdir=tofu apply
hcloud server describe 132168416 | grep -i name     # -> llunde-01
```

Stale S3 lock from a killed apply: `tofu force-unlock <id>` — only after confirming nothing is
actually running.

## 3. Host identity & secrets — BEFORE install

The host's SSH key is created *by us* and injected at install, so sops decrypts from first boot
(ADR 007).

1. Generate it locally (kept only until injected, then deleted):
   ```sh
   mkdir -p /tmp/llunde-01-keys/etc/ssh
   ssh-keygen -t ed25519 -N "" -C llunde-01 -f /tmp/llunde-01-keys/etc/ssh/ssh_host_ed25519_key
   ssh-to-age < /tmp/llunde-01-keys/etc/ssh/ssh_host_ed25519_key.pub   # -> <HOST_AGE_PUBLIC_KEY>
   ```
   Host already running? `ssh-keyscan -t ed25519 46.62.214.182 | nix run nixpkgs#ssh-to-age`.
2. Add `<HOST_AGE_PUBLIC_KEY>` to `.sops.yaml` as the host's recipient, then
   `sops updatekeys secrets/<name>.yaml` for every file it must read. Which host decrypts what is
   the `creation_rules` list there.
3. Create the host's secret files — `sops secrets/<name>.yaml` opens an editor; keys are defined
   in `modules/secrets/default.nix`, `ls secrets/` shows the current set. Provenance where it
   isn't obvious:
   - `secrets/doppler.yaml` — key `doppler_token`, **env-file form** (it lands as an
     EnvironmentFile): `DOPPLER_TOKEN=<token>`, from
     `doppler configs tokens create llunde-01 --project llunde --config prd --plain --max-age 0`
   - `secrets/tailscale.yaml` — key `auth_key`, from the Tailscale admin console → Settings →
     Keys → *Auth keys* → Generate (reusable: no, ephemeral: no, tags optional).
   - `secrets/restic.yaml` — `password` (`openssl rand -base64 32`) and `env` in env-file form:
     `AWS_ACCESS_KEY_ID=…`, `AWS_SECRET_ACCESS_KEY=…`, `AWS_DEFAULT_REGION=eu-north-1`. Reuses the
     `leploy` credentials from Doppler `pyparser/prd` (object-level S3 rights suffice); a
     dedicated backup IAM user is a hardening follow-up.
   - `secrets/llunde-backend-db.yaml` — key `env`, env-file form:
     `POSTGRES_PASSWORD`/`DB_PASSWORD` = `openssl rand -base64 24` (same value), optional
     `VALKEY_PASSWORD`.
   - `secrets/ghcr.yaml` — key `auth_json` (images are PRIVATE). Mint a fine-grained PAT
     (github.com/settings/tokens → read-only `packages`; no repo perms needed for classic
     `read:packages`); the value is the literal JSON
     `{"auths":{"ghcr.io":{"auth":"$(echo -n 'fredrir:<PAT>' | base64)"}}}`. Verify on-host later:
     `REGISTRY_AUTH_FILE=/run/secrets/ghcr-auth.json podman pull ghcr.io/fredrir/llunde-frontend:latest`.
4. Mirror `DB_PASSWORD` (and `VALKEY_PASSWORD` if set) into Doppler `llunde/prd`.
5. Commit — ciphertext only; `git diff --cached` must show nothing but `sops`-encrypted content.

## 4. Install NixOS — 💥 DESTROYS THE BOX

Everything on the target disk goes (ADR 001). Preconditions: §2 clean, §3 committed,
`ssh root@46.62.214.182` works.

```sh
nix run github:nix-community/nixos-anywhere -- \
  --flake .#llunde-01 \
  --build-on-remote \
  -i ~/.ssh/id_ed25519 \
  --extra-files /tmp/llunde-01-keys \
  root@46.62.214.182
```

`--build-on-remote` is required (laptop aarch64-darwin, target x86_64-linux). nixos-anywhere
kexecs into an installer, runs disko (single-disk ext4 wipe of `/dev/sda`), installs the flake's
system, copies `--extra-files` (the host key) into place, reboots.

⚠️ **The installer has no tailscaled** — it needs public 22, so an install aimed at a tailnet IP
dies mid-flight. §12 has the temporary opening and how to close it again.
⚠️ Verify the exact `-i` / `--extra-files` flag spellings against the nixos-anywhere version
actually pulled.

Afterwards `rm -rf /tmp/llunde-01-keys` — the key now lives only on the host — and
`ssh root@46.62.214.182` must present the ed25519 fingerprint you generated.

## 5. Verify first boot & tailnet join

```sh
ssh root@46.62.214.182 systemctl --failed          # expect: 0 loaded units listed
ssh root@46.62.214.182 ls /run/secrets/            # expect the host's secrets materialized
ssh root@46.62.214.182 tailscale status            # expect: joined, hostname llunde-01
tailscale status | grep llunde-01                  # from the laptop -> <TAILNET_IP> (100.x.y.z)
ssh root@<TAILNET_IP> true                         # management path works (ADR 008)
```

## 6. Deploy — the pull loop (ADR 020)

**Merging to `main` IS the deploy.** `Check` passes → `promote.yml` fast-forwards `deploy` → each
host applies itself (`modules/gitops-pull`): **llunde-01** within ~5 min of promotion (the
canary), **llunde-parser** +30 min behind (it refuses younger revs).

```sh
ssh root@<TAILNET_IP> journalctl -fu gitops-pull        # watch a rollout
ssh root@<TAILNET_IP> cat /var/lib/gitops-pull/applied  # which rev is live
```
(also `gitops_pull_applied_info` / `gitops_pull_last_success_timestamp` in Prometheus)

Units whose quadlet file or bind-mounted config changed (Caddyfile, cloudflared, prometheus.yml,
…) are restarted by the reconcile map (`llunde.gitopsPull.reconcile` in each host file). **A NEW
quadlet unit or bind-mounted config MUST be added to that map** — otherwise its config lands on
disk but never goes live: NixOS's switch only daemon-reloads user managers, and podman resolves
bind mounts at container *creation*.

**Manual deploy — the recovery route, must always work:**

```sh
ssh root@<TAILNET_IP> systemctl stop gitops-pull.timer   # pause the loop FIRST
nix run nixpkgs#nixos-rebuild -- switch --flake .#<host> \
  --target-host root@<TAILNET_IP> --build-host root@<TAILNET_IP>
# ...restart affected user units per the reconcile map, then:
ssh root@<TAILNET_IP> systemctl start gitops-pull.timer
```

⚠️ **The loop converges to `deploy`**: a manually deployed rev that is NOT the `deploy` tip gets
rolled back to it on the next cycle. Keep the timer stopped until your change is merged **and
promoted**.

⚠️ **A rev that changes BOTH a reconcile `check` command AND the config that check validates
gets graded by the OLD check.** gitops-pull runs from the RUNNING generation
(`restartIfChanged = false`, the self-deploying-deployer guard), so a new check string only takes
effect on the NEXT run. Seen in production: a Caddyfile that gained `acme_dns` was validated by
the previous `pkgs.caddy` check, which lacked the DNS provider — restart skipped, sticky
`reconcile_failed`, old container still serving. **Land a check change in its own rev first** (a
no-op against the old config), then the config change. Recovery if it bites: run the new check by
hand, restart the unit, `rm /var/lib/gitops-pull/reconcile_failed`, `systemctl start gitops-pull`.

## 7. The public edge — records, firewalls, verification, break-glass (ADR 017)

`llunde.no`, `www.llunde.no` and `api.llunde.no` are **proxied CNAMEs onto the tunnel**
(`<tunnel>.cfargotunnel.com`); zone `llunde.no` = `4ae54b24fc4140d4d1c450491645f1c8`. No A or
AAAA record points at a host. Both firewall layers — Hetzner cloud and NixOS — are closed to
80/443, and **Caddy still binds those ports behind them with certificates kept warm over DNS-01**,
so break-glass is one rule away and never waits on Let's Encrypt. History: ADR 017. Tofu env: §2.

### 7.1 The records

Tofu-managed in `tofu/modules/cloudflare/`. Two constraints bite anyone editing them:

- **A CNAME cannot coexist with an A *or* AAAA record at the same name**, and tofu gives **no
  ordering guarantee** between an unrelated create and destroy in one apply. Moving a name onto
  the tunnel therefore takes **two applies**: drop AAAA on its own first, then flip A → CNAME. As
  one apply it can die on Cloudflare error 81053 ("An A, AAAA, or CNAME record with that host
  already exists") — and *may* pass once and fail on a re-run.
- The `moved {}` block keeps the same resource address, so tofu updates existing record ids **in
  place** instead of create-before-destroy.

Between those two applies IPv6-only clients lose the site (minutes); CF restores v6 at the edge on
the flip, so v6 reachability is net better afterwards. Proxied records propagate in seconds (CF
answers authoritatively; TTL `auto`, 300 s) — budget **≤5 min** for resolver caches; anything
still resolving `46.62.214.182` after that is a stale local resolver, not a failure.
**Undo**: `git revert` the PR, `tofu apply`.

### 7.2 Edge verification — the reusable battery

Run after any edge change, and top-to-bottom during break-glass recovery. **Stop and roll back on
the first failure.**

```sh
# 1. nothing resolves to a host address
dig +short llunde.no www.llunde.no api.llunde.no      # CF anycast (104.21.x/172.67.x), NOT 46.62.214.182
for h in llunde.no www.llunde.no api.llunde.no; do dig +short AAAA "$h"; done   # no host addresses

# 2. traffic rides the CF edge — what blackbox-cfray asserts continuously; it goes red the
#    moment traffic stops riding the edge, which is its whole job
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

# 5. the real-IP contract, end to end (ADR 017's non-optional verification).
#    NB: there is NO audit table and the backend logs no client IPs (schema is
#    flyway_schema_history + users). Do not go looking for an audit row — that check cannot be
#    run. The RATE LIMITER is the observable proof, and a better one: it exercises the same
#    keying the buckets actually use.
EMAIL="edge-$(date +%s)@example.com"
curl -sS -i -X POST https://api.llunde.no/auth/register -H 'Origin: https://llunde.no' \
  -H 'X-CSRF-Token: t' -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"correct horse battery\"}"          # 201
curl -sS -i -X POST https://api.llunde.no/auth/login -H 'Origin: https://llunde.no' \
  -H 'X-CSRF-Token: t' -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"correct horse battery\"}" | grep -i set-cookie
#    -> 200, and the cookie carries `Secure` — proof that X-Forwarded-Proto=https survives the
#       plain-HTTP tunnel hop (registration alone sets no cookie).

# 20 failed logins, each with a DIFFERENT forged X-Forwarded-For:
for i in $(seq 1 20); do
  curl -sS -o /dev/null -w '%{http_code} ' -H "X-Forwarded-For: 203.0.113.$i" \
    -H 'Origin: https://llunde.no' -H 'X-CSRF-Token: t' -H 'Content-Type: application/json' \
    -X POST https://api.llunde.no/auth/login -d '{"email":"nobody@example.com","password":"wrong"}'
done; echo
#    -> a few 401s then 429s (observed: 401 x5, then 429 x15). All 20 share ONE bucket despite
#       rotating forged XFF — only possible if Caddy REPLACED X-Forwarded-For with
#       CF-Connecting-IP and discarded the client's. If forged XFF were trusted, each request
#       would be its own bucket and no 429 would ever appear.

# 6. the collectors agree
curl -s http://100.92.219.50:9090/api/v1/targets | jq -r '.data.activeTargets[] | "\(.labels.job) \(.health)"' | sort
#    blackbox-cfray -> up AND probe_success 1
```

### 7.3 The Hetzner cloud firewall

`llunde-fw` and llunde-parser's firewall (`tofu/parser-server.tf`) both carry **no rules**.

⚠️ **The hcloud provider cannot delete a firewall's last rules.** It omits the rules field
entirely, silently no-ops, and prints "Apply complete" — a lie. Go to zero out-of-band, then
reconcile state:

```sh
hcloud firewall describe llunde-fw                       # what is actually there
echo '[]' | hcloud firewall replace-rules --rules-file - llunde-fw
hcloud firewall describe llunde-fw                       # Rules: (empty)

tofu -chdir=tofu apply -refresh-only     # state learns the firewall has no rules
tofu -chdir=tofu plan                    # "No changes." once the HCL has no rule blocks
```

⚠️ **`--rules-file` takes `-` for stdin — use it.** `--rules-file <([])` is a typo: `[]` is not a
command, so the process substitution hands hcloud an **empty file** while the shell still exits 0.
`<(echo '[]')` works, but piping to `-` is unambiguous across shells with no
process-substitution trap at all.

🚧 **Never run a plain `tofu apply` while the HCL still declares rules you removed by hand** — it
re-opens them at the cloud edge, quietly undoing the step. `-refresh-only` is safe; a full apply
is not, until the rule blocks are gone from `tofu/llunde-firewall.tf`.

**Re-open** — break-glass; seconds, and it works with the tailnet down. The firewall name is
positional and goes last:

```sh
hcloud firewall add-rule --direction in --protocol tcp --port 80  --source-ips 0.0.0.0/0,::/0 llunde-fw
hcloud firewall add-rule --direction in --protocol tcp --port 443 --source-ips 0.0.0.0/0,::/0 llunde-fw
```

Check the posture from **off** the tailnet:

```sh
curl -sS --max-time 8 http://46.62.214.182/  ; echo "exit=$? (28 = timeout = closed)"
curl -sS --max-time 8 https://46.62.214.182/ ; echo "exit=$? (28 = timeout = closed)"
curl -s -o /dev/null -w '%{http_code}\n' https://llunde.no   # 200 — the tunnel path is unaffected either way
```

### 7.4 The NixOS host firewall

`llunde.profile.publicTCPPorts` in `hosts/llunde-01/default.nix` opens nothing; changes here ride
the pull loop (§6). **Both layers must be open for public traffic to arrive** — cloud firewall
*and* NixOS firewall; either alone still blocks.

The firewall here is **iptables, not nftables** (`nft list ruleset` is empty), the chain is
`nixos-fw`, and both families must be checked:

```sh
ssh root@100.109.80.121 'iptables -S nixos-fw | grep -E "dport (80|443)"'   # no output
ssh root@100.109.80.121 'ip6tables -S nixos-fw | grep -E "dport (80|443)"'  # no output
ssh root@100.109.80.121 'ss -ltn | grep -E ":(80|443) "'  # Caddy STILL BINDS them — the firewall is what closed
ssh root@100.109.80.121 cat /var/lib/gitops-pull/applied  # == the rev you expect
ssh root@100.92.219.50  cat /var/lib/gitops-pull/applied  # parser follows +30 min
```

**Changing these ports cannot trigger the deadman.** Its probes are management-plane only —
tailnet online (or peer ping), `sshd` listening on 22, `git ls-remote` fetchable — and none
traverses 80/443. `tailscale0` stays a trusted interface, so SSH is unaffected. (Read out of
`modules/gitops-pull/default.nix`, not assumed.)

**Undo**: `git revert` + merge — auto-applies in ~5 min. Faster, or if the gate is red:
`git push -f <good-sha>:deploy` (§9). Neither restores the *cloud* firewall — §7.3's `add-rule`
is the lever that actually matters.

### 7.5 Full break-glass — the tunnel or Cloudflare itself is broken

Certs are warm, so this is DNS + firewall only, **in this order**:

1. `hcloud firewall add-rule` ×2 (§7.3) — the origin can answer again.
2. `git revert` the commit that emptied `publicTCPPorts` + merge — the NixOS firewall follows
   within ~5 min. Steps 1 and 2 are both needed.
3. `git revert` the A → CNAME commit + `tofu apply` — A records return, grey-cloud.
4. Optionally restore the AAAA records too.

Neither Caddy nor the CF edge sets HSTS, so browsers can click through any interim TLS error —
the window is degraded, not hard-failed. Step 3 needs the Cloudflare **DNS API**; it does *not*
need the CF proxy or the tunnel, which is exactly the outage this path is for.

## 8. Verification battery — is the estate actually serving?

Run after a significant change, and as the acceptance check on a rebuilt host. Edge-specific
checks are §7.2.

```sh
curl -s https://api.llunde.no/ready                        # {"database":true,"valkey":true}
curl -s -o /dev/null -w '%{http_code}\n' https://llunde.no # 200 (frontend)
for p in metrics health ready; do curl -s -o /dev/null -w "$p %{http_code}\n" https://api.llunde.no/$p; done
                                                           # blocked publicly (403/404) — except /ready if the Caddyfile deliberately allows it
curl -s http://<TAILNET_IP>:<NODE_EXPORTER_PORT>/metrics | head -1   # reachable over tailnet only
# auth lifecycle (backend, against prod):
EMAIL="gate-$(date +%s)@example.com"
curl -s -X POST https://api.llunde.no/auth/register -H 'Origin: https://llunde.no' -H 'X-CSRF-Token: t' \
  -H 'Content-Type: application/json' -d "{\"email\":\"$EMAIL\",\"password\":\"correct horse battery\"}"   # 201
# ...login/me/sessions/logout-all as in the backend runthrough. For the real-client-IP half use
# §7.2's rate-limiter probe — there is no audit log to read.
# isolation:
ssh root@<TAILNET_IP> "sudo -u llunde-frontend curl -s --max-time 3 http://llunde-postgres:5432 || echo ISOLATED"
# auto-update: push a trivial backend image change to main, wait for the timer (or poke:
ssh root@<TAILNET_IP> systemctl --user -M llunde-backend@ start podman-auto-update.service
# backups: force one run, list snapshots, restore one dump into a scratch db:
ssh root@<TAILNET_IP> systemctl start restic-backups-llunde-backend.service   # upstream restic module naming
# reboot test:
ssh root@<TAILNET_IP> reboot && sleep 90 && curl -s https://api.llunde.no/ready
```

## 9. Update & rollback

- **App images**: per-user `podman-auto-update.timer` polls GHCR `:latest`
  (`AutoUpdate=registry`). Manual poke: §8's
  `systemctl --user -M <user>@ start podman-auto-update.service`. Pin or roll back: set the
  unit's `Image=` to a digest in `services/<name>/default.nix`, deploy (§6).
- **Config — roll back the estate**: `git push -f <good-sha>:deploy` from the laptop. Works with
  the tailnet down (`deploy` is deliberately unprotected so this lever always exists); hosts
  converge within a cycle (llunde-01) / after the 30-min lag (parser). Then fix forward on main
  via PR — the next green merge fast-forwards `deploy` back onto main's history.
- **Pause all applies**: GitHub → Actions → *Promote to deploy* → Disable workflow (freezes
  `deploy`, both hosts hold). Per host: `systemctl stop gitops-pull.timer`.
- **Gate wedged by a GitHub outage, deploys blocked** (precedent 2026-08-12): the `tofu` job
  failed twice fetching provider `SHA256SUMS` from GitHub release assets (503, then a timeout)
  while the status page read "Actions: Normal" — release-asset delivery is a different component.
  A red gate means NO deploys, so this freezes the estate. `deploy` was advanced by hand:
  `git push origin <sha>:deploy`. **Only with evidence the gate is lying** — there, main's TREE
  was byte-identical to a PR head whose Check was fully green, the diff touched no tofu at all,
  and `nix flake check` + `tofu fmt` + `tofu validate` were re-run locally on the merged commit
  first. This BYPASSES ADR 020's gate-green invariant; it is a break-glass **lever, not a
  shortcut**. The gate's provider cache makes it much less likely to recur.
- **After a deadman rollback** (host rebooted into the previous generation; you got the
  healthchecks `/fail` email): the offending rev is **HELD** on that host — no reboot loop. Fix
  forward on main; the newly promoted rev clears the hold automatically. To force-retry the same
  rev: `rm /var/lib/gitops-pull/attempting` on the host.
- **Sticky reconcile failure** (`/var/lib/gitops-pull/reconcile_failed` exists, heartbeat failing
  while "converged"): a unit restart failed after a switch. Fix the unit; any successful apply
  clears the marker (or remove it by hand).
- **Host unreachable and the deadman didn't fire**: `hcloud server reset <name>` — boots the
  previous boot-default generation (the loop never moves the boot default before its probes pass).
- **Host won't BOOT** (kernel/initrd/bootloader — the accepted boot-plane residual, ADR 020):
  Hetzner console (web VNC) → systemd-boot menu → select the previous generation.
- **Emergency local rollback** (on a reachable host): `nixos-rebuild --rollback switch` still
  works — but the loop re-converges to `deploy` unless you stop the timer.

## 10. Restore from backup

```sh
export RESTIC_REPOSITORY="s3:s3.eu-north-1.amazonaws.com/llunde-pyparser-bucket/<RESTIC_PREFIX>"   # from modules/backups
restic snapshots            # password + AWS env from secrets/restic.yaml values
restic restore latest --target /tmp/restore
# postgres (dump file path per the backup preHook):
ssh root@<TAILNET_IP> "podman exec -i -u postgres llunde-postgres psql -U llunde llunde" < /tmp/restore/<DUMP_PATH>
# valkey: stop unit, replace appendonly dir from restore, start unit
```

## 11. Access, break-glass SSH & the public-port posture (ADR 015 + ADR 017)

**The estate has ZERO public inbound ports.** Every SSH consumer rides the tailnet: laptop
(`ssh root@llunde-01` / `root@llunde-parser.tail0b6cbe.ts.net`), pyparser CI, portfolio CI (each
joins per-run with an ephemeral `tag:ci` key). All web ingress arrives through the Cloudflare
tunnels the hosts dial outbound.

**Re-opening 80/443** — the web half of break-glass; full sequence in §7.5. Caddy never stopped
binding those ports and its certs are valid (DNS-01 renewal never needed an inbound port), so the
origin serves the moment the rule lands. The NixOS half is a `git revert` of the commit that
emptied `publicTCPPorts`; both layers must be open.

```sh
hcloud firewall add-rule --direction in --protocol tcp --port 80  --source-ips 0.0.0.0/0,::/0 llunde-fw
hcloud firewall add-rule --direction in --protocol tcp --port 443 --source-ips 0.0.0.0/0,::/0 llunde-fw
```

**Break-glass, per host** (tailnet down or node expired):

1. Hetzner Cloud console (web VNC) — always works, no network path needed.
   `hcloud server request-console <name>` or the dashboard. Root password login is disabled; use
   the console for single-user/rescue boot, or:
2. `hcloud server enable-rescue <name> && hcloud server reset <name>` — boots the rescue system
   with your Hetzner SSH key on public 22. Rescue is a different boot target but is still gated by
   the cloud firewall, so pair it with step 3.
3. Re-open 22 temporarily: add the `port = "22"` rule back in `tofu/llunde-firewall.tf`
   (llunde-01) or `tofu/parser-server.tf` (llunde-parser), `tofu apply`. **llunde-parser only**:
   also `ufw allow 22` once you're in. Revert both when done — the closed state is the committed
   one.

llunde-01's NixOS host firewall does not list 22 for the public interface
(`modules/profiles/server.nix`); `tailscale0` is a trusted interface, so sshd stays reachable over
the tailnet regardless.

## 12. `llunde-parser` — per-host deltas

Same runbook, second host. Only the differences from §§2–10.

- **§2 Provision — nothing to do.** The server exists (Hetzner 141119325, CCX23, adopted in
  tofu); a reinstall needs no tofu change and `tofu plan` must stay "No changes." throughout.
- **§3 Identity & secrets**: host key pre-generated at
  `/tmp/llunde-parser-keys/etc/ssh/ssh_host_ed25519_key`. Own files:
  `secrets/pyparser-doppler.yaml`, `secrets/pyparser-restic.yaml`. Shared with llunde-01:
  `secrets/tailscale.yaml`, `secrets/ghcr.yaml` (`sops updatekeys` after adding the host recipient
  to `.sops.yaml`'s per-host creation rules). No DB-password sops file — `POSTGRES_PASSWORD` is
  single-sourced from the Doppler `pyparser/prd` render.
- **§4 Install** (💥 destroys the box — both tenants down for the window):
  ```sh
  nix run github:nix-community/nixos-anywhere -- \
    --flake .#llunde-parser \
    --build-on-remote \
    -i ~/.ssh/id_ed25519 \
    --extra-files /tmp/llunde-parser-keys \
    root@<LLUNDE_PARSER_IP>
  ```
  ⚠️ **CRITICAL, before the new install's `tailscale up`** (i.e. immediately after the wipe
  starts): free the `llunde-parser` MagicDNS name. From the old box itself,
  `tailscale set --hostname llunde-parser-old` — the record renames live, the name frees instantly
  (verify: the old FQDN stops resolving), and the corpse record can be deleted from the console at
  leisure. Skip this and the new box joins as `llunde-parser-1` while every consumer (portfolio's
  `DEPLOY_HOST`, ssh aliases) resolves a corpse.

  ⚠️ **The installer needs public 22** — after kexec the box runs the nixos-anywhere installer,
  which has no tailscaled: an install over the tailnet IP dies mid-flight. Temporarily
  `hcloud firewall add-rule` 22 (plus `ufw allow 22` while the old OS is still Ubuntu), install
  against the **public** IP, then take the firewall back to zero-inbound with `--rules-file -`
  (§7.3). The NixOS host firewall opens nothing regardless.
- **§5 Verify**: same checks with hostname `llunde-parser` (secrets in `/run/secrets/`, tailscale
  joined as **exactly** `llunde-parser`). Plus `ss -tlnp` showing no public listeners at all — not
  even 80/443 (ingress is the Cloudflare tunnel, outbound) — and both tenants:
  `systemctl --user -M pyparser@ list-units 'pyparser-*'` for the stack; `id portfolio` (uid 3000)
  + `grep portfolio /etc/subuid` (100000:65536) + linger for the slot.
- **§6 Deploy loop**: identical, with `.#llunde-parser` and this host's tailnet address as
  `--target-host`/`--build-host`. Quadlet check:
  `systemctl --user -M pyparser@ list-units 'pyparser-*'`.
- **§7 mostly does not apply** — both its hostnames were always on the pyparser tunnel and it has
  had zero public inbound throughout. **§13 does apply**, with the pyparser tunnel id
  (`e77d6ebf-…`), uid 2001, and the `:latest`/shared-network connector shape. **§8**'s hostnames
  here are `parser.llunde.no`, `external.llunde.no`, `hansteen.dev`.
- **§9 Update & rollback**: same pull-based model; the pyparser digest-pin lever lives in
  `services/pyparser/`. Details in [docs/pyparser/PROD.md](pyparser/PROD.md).
- **§10 Restore**: `<RESTIC_PREFIX>` = `restic/llunde-parser`. pyparser only —
  `restic restore latest --target /tmp/restore`, then the custom-format dump goes through
  `pg_restore` (not psql):
  ```sh
  runuser -u pyparser -- env XDG_RUNTIME_DIR=/run/user/2001 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2001/bus \
    podman exec -i pyparser-postgres pg_restore --clean --if-exists \
    -U pyparser -d pyparser_llunde < /tmp/restore/var/backup/pyparser/pyparser_llunde.dump
  # files: copy the restored pyparser-files/_data contents back into the volume
  ```
  **portfolio restores itself** from its own S3 backups per its own DR runbooks — llunde-infra
  hands off at the slot boundary (ADR 016).

## 13. cloudflared & the tunnel edge — ops (ADR 017)

The llunde tunnel is **`c0cdd9b5-fa97-42a1-bca7-95da236ea949`**, terminated by the `cloudflared`
quadlet under `edge` (uid 2000) on llunde-01: `Network=host`, **digest-pinned**, token from
`/run/secrets/llunde-tunnel.env`. It dials Caddy's plain-HTTP `:8085` loopback listener;
Cloudflare terminates public TLS. Do **not** confuse it with pyparser's tunnel `e77d6ebf-…`
(parser/external hostnames, llunde-parser) — that one is not ours to touch.

⚠️ **Never rotate the tunnel token casually.** A tunnel's token embeds its secret; regenerating it
drops the live connectors — i.e. the front door — until the new token is deployed through sops +
a switch.

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

`runuser` needs a cwd the target user can read — hence the `cd /`. Without it you get
`cannot chdir to /root: Permission denied`, which looks like a podman failure and is not.

### 13.2 What ingress map is the tunnel actually serving?

The hostname→service map is **remote** (Cloudflare-side) config the connector fetches at startup
and on change. The connector's own log is ground truth for what it is running right now — the
dashboard is not the only place to look:

```sh
ssh root@100.109.80.121 'cd / && runuser -u edge -- env XDG_RUNTIME_DIR=/run/user/2000 \
  DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2000/bus \
  journalctl --user -u cloudflared --no-pager | grep "Updated to new configuration" | tail -1'
```

The contract — all three vhosts land on the one Caddy listener, which routes by `Host`:

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

A `401`/`10000 Authentication error` here means you are holding the **old DNS-only** ops token —
not that the tunnel is broken.

### 13.3 Changing the connector

**Ingress map** — tofu-managed (`tofu/modules/cloudflare/`); a dashboard edit is drift, reverted
by the next apply. Changes reach the running connector in seconds without a restart (it
re-fetches — watch for a new `Updated to new configuration … version=N` line).

**The unit** (image digest, memory) — NixOS-declared: edit `modules/ingress/`, regenerate
`tests/golden/cloudflared.container`, PR → merge → the pull loop restarts it via the reconcile map
(`hosts/llunde-01/default.nix`). The front door never auto-updates: `autoUpdate = false` for
`edge` is deliberate; image bumps are deliberate digest edits.

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
| `400` on everything through the tunnel | the `CF-Connecting-IP` guard: no such header means the request did not come from CF | expected for a direct hand-probe of `:8085`; add `-H 'CF-Connecting-IP: 1.2.3.4'` to test |
| Rate limits key on one bucket / logs show `127.0.0.1` | the XFF←`CF-Connecting-IP` mapping was lost | `tests/golden/llunde-01.Caddyfile` diff; it cannot regress silently |
| Cloudflare itself is down | — | §7.5 full break-glass |

### 13.5 `OriginCertExpiring` — the DNS-01 renewal stopped working

**Read this before touching anything: the site is NOT down.** Public traffic never sees Caddy's
certificates — cloudflared dials `:8085` in plain HTTP and Cloudflare presents its own edge
certificate. What this alert says is that **break-glass has gone cold**: §7.5 and §11 both assume
re-opening 80/443 serves valid TLS immediately, and that assumption is the thing expiring.

That invisibility is why the alert exists. No other probe in the estate looks at an origin
certificate (`probe_ssl_earliest_cert_expiry` on the `blackbox-public*` jobs is Cloudflare's edge
cert, not this one), so a renewal failure would otherwise surface only at the moment break-glass
needed it. The stamp comes from `caddy-cert-expiry.timer` on llunde-01 (hourly, `modules/ingress`),
lands as a node_exporter textfile metric, and fires at **21 days remaining** — Caddy starts
renewing at ~30, so crossing 21 means renewal has been failing for over a week.

```sh
# what the alert is reading
ssh root@100.109.80.121 'cat /var/lib/node-exporter-text/caddy_cert_expiry.prom'

# ground truth, the same handshake break-glass would get
ssh root@100.109.80.121 "curl -sSv --max-time 8 --resolve llunde.no:443:127.0.0.1 \
  https://llunde.no/ -o /dev/null 2>&1 | grep -E 'subject:|expire date|issuer:'"

# run the stamp by hand (it exits non-zero if a name serves no certificate)
ssh root@100.109.80.121 'systemctl start caddy-cert-expiry && \
  journalctl -u caddy-cert-expiry -n 20 --no-pager'
```

Then find out why renewal failed — the answer is nearly always the DNS-01 token:

```sh
# Caddy's own account of it
ssh root@100.109.80.121 'cd / && runuser -u edge -- env XDG_RUNTIME_DIR=/run/user/2000 \
  DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2000/bus \
  journalctl --user -u caddy --no-pager | grep -iE "obtain|renew|acme|challenge" | tail -40'

# the token must be materialized AND still valid at Cloudflare
ssh root@100.109.80.121 'test -s /run/secrets/llunde-caddy-acme.env && echo "secret present"'
```

| Symptom in the caddy log | Cause | Move |
|---|---|---|
| `dns` challenge, `Authentication error (10000)` | the host-scoped `Zone:DNS:Edit` token was revoked or expired | mint a new one, `sops secrets/llunde-caddy-acme.yaml`, PR → merge → the reconcile map restarts caddy |
| `no solvers available` / falls back to `http-01` | the running image lost the DNS provider — i.e. the digest moved to a plugin-less build | `tests/golden/caddy.container` diff; CI's `verify` job asserts `dns.providers.cloudflare` on every build |
| Renewals never attempted at all | `acmeDnsTokenFile` is null, so `acme_dns` never rendered | `tests/golden/llunde-01.Caddyfile` must contain `acme_dns cloudflare` |

**If it is going to expire before you can fix it**, the fallback is the old path: re-open 80/443
(§11), which restores an http-01 route Let's Encrypt can use. That is a deliberate, temporary
retreat from zero-public-inbound — close it again the moment DNS-01 issues cleanly.
