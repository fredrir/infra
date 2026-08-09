# Phase 3 — task breakdown

Ground rule carried over from phase 2: **declarative files are authored freely; live surfaces (tofu applies, the llunde-parser box, DNS, tailnet ACLs) are only ever touched serially by the lead.** Two live productions share `llunde-parser` — every step names its rollback before it runs.

Cross-repo surface: three repos change in this phase — llunde-infra (tofu, docs), **llunde-pyparser** (its first `.github/`), **portfolio** (Tailscale step in `deploy.yml`). The portfolio PR is written purely in that repo's own terms.

## Step 1 — pyparser state → S3 (lead)

| # | Task | Proof | Rollback |
|---|---|---|---|
| 1.1 | Baseline: `tofu plan` in `tofu/pyparser` with local state → expect **"No changes."** Copy `terraform.tfstate` aside as a dated backup | plan output kept | n/a (read-only) |
| 1.2 | Uncomment the S3 backend stub in `versions.tf` (bucket `llunde-pyparser-bucket`, key `tofu-state/pyparser.tfstate`, `use_lockfile = true` per the stub); `tofu init -migrate-state` | init reports migration complete | local state backup from 1.1; re-comment the backend, `init -migrate-state` back |
| 1.3 | Post-migration `tofu plan` → **"No changes."**; delete the local tfstate only after the S3 object is confirmed present | plan output + `aws s3 ls` | backup from 1.1 |

## Step 2 — Cloudflare zone into tofu (lead; author + import)

| # | Task | Proof | Rollback |
|---|---|---|---|
| 2.1 | Scoped CF API token (Zone:DNS edit + Zone read, `llunde.no` only) created in the dashboard, stored in Doppler; never in git | — | revoke token |
| 2.2 | Author `tofu/modules/cloudflare/` + wiring from the llunde root: provider **v5** (`cloudflare_dns_record` — the v4 name is gone), records: `llunde.no` A/AAAA, `www` A/AAAA, `api` A/AAAA (all grey), `parser` + `external` proxied CNAMEs → the pyparser tunnel. **No tunnel/Access resources** (`tofu/pyparser/CLOUDFLARE.md` warning) | `tofu validate` | files revert cleanly — nothing applied yet |
| 2.3 | Declarative `import` blocks for every existing record; `tofu plan` → imports only, no replaces; apply | post-apply `plan` → "No changes."; `dig` answers identical before/after | records exist regardless — worst case remove from state (`tofu state rm`), zone untouched |
| 2.4 | Audit the zone listing vs code: every record accounted for; **verify the old llunde tunnel (`ed8abcdb…`) is deleted**, delete it if still present (it carries no traffic — DNS left it in phase 2) | zone export matches code; tunnel list has only `pyparser-review` | tunnel deletion is the cleanup itself; nothing depends on it |

## Step 3 — llunde-parser joins the tailnet (lead, on-box; touches no tenant)

| # | Task | Proof | Rollback |
|---|---|---|---|
| 3.1 | Reusable/tagged Tailscale auth key; `apt install tailscale`, `tailscale up` on llunde-parser. No firewall or tenant change yet | `tailscale status` on both ends | `tailscale down` + apt remove — box returns to exactly prior state |
| 3.2 | Prove root SSH over the tailnet name; repoint the laptop `letzner` alias; run one `pyparser-sync db diff dev prod` over it (its SSH tunnel rides the alias) | interactive login + sync command succeed | alias points back at the public IP (still open) |
| 3.3 | Tailnet ACLs: define `tag:ci`; CI-tagged ephemeral nodes may reach only the two hosts on 22; mint an **ephemeral** auth key for CI use (stored in Doppler, one per consumer: `pyparser/ci`, portfolio's secret store) | ACL test in the admin console | ACL revert; keys revocable individually |

## Step 4 — pyparser CI recreation (author in llunde-pyparser; lead merges)

Faithful port of [`old.deploy-pyparser.yml`](../../../research/old-workflows/old.deploy-pyparser.yml) — mechanics preserved, three adjustments: repo-root context, tailnet transport, current repo name.

| # | Task | Proof | Rollback |
|---|---|---|---|
| 4.1 | `.github/workflows/deploy.yml`: build job unchanged in spirit (`target: review-serve`, GHA cache, tags `:sha` + `:latest`, `ghcr.io/fredrir/pyparser-review`) but `context: .`; paths filter drops the `pyparser/**` prefix; same `concurrency` group; same `workflow_dispatch` `image_tag` input | PR review against the old file side-by-side | repo had no CI — removing the file restores status quo |
| 4.2 | Deploy job: Tailscale action joins with the `tag:ci` ephemeral key (from Doppler `pyparser/ci`, like the SSH key), then the existing ship-files + run-`deploy-remote.sh` steps against the **tailnet** hostname | — | same |
| 4.3 | Keep-10 GHCR prune job verbatim | — | same |
| 4.4 | Prove: merge a no-op change → full pipeline (build → tailnet SSH → drain/migrate/health) goes green, `parser.llunde.no` stays up; then one `workflow_dispatch` with the previous `image_tag` → rollback lever proven | two green runs + site checks | `deploy-remote.sh`'s own ERR-trap rollback covers a bad deploy; dispatch with prior tag covers a bad image |

## Step 5 — portfolio deploys over the tailnet (small PR to portfolio; owner merges)

| # | Task | Proof | Rollback |
|---|---|---|---|
| 5.1 | PR: Tailscale step in `deploy.yml`'s deploy job (ephemeral `tag:ci` key from its own secret store); `DEPLOY_HOST` → tailnet name; `DEPLOY_KNOWN_HOSTS` updated to match. Written purely as "deploy over Tailscale" — no references to anything outside that repo | PR review | revert the PR; public 22 still open until step 6 |
| 5.2 | Prove: one real deploy end-to-end (forced-command path unchanged); synthetic checks stay green | green deploy + `hansteen.dev` checks | same |

## Step 6 — close port 22 everywhere (lead, serial; only after 3.2, 4.4, 5.2 are green)

| # | Task | Proof | Rollback |
|---|---|---|---|
| 6.1 | Runbook: break-glass section rewritten first — Hetzner console/rescue login path, and the tofu one-liner that re-opens 22 | doc review | n/a |
| 6.2 | llunde-01: drop 22 from `llunde-fw` (tofu) and from the NixOS firewall (`modules/profiles/server.nix` — tailscale0 stays trusted, so tailnet SSH is unaffected); apply, rebuild | public `ssh -o ConnectTimeout=5` times out; tailnet SSH works; sites up | tofu re-add + rebuild; worst case Hetzner console |
| 6.3 | llunde-parser: drop 22 from the pyparser firewall (tofu) **and** `ufw delete allow 22` on the box (fail2ban left as-is — it just goes quiet) | same checks + **both CI deploys re-run green post-closure** | tofu re-add; ufw re-allow via Hetzner console if tailnet ever lost |

## Step 7 — tofu unnesting (lead)

| # | Task | Proof | Rollback |
|---|---|---|---|
| 7.1 | Merge the two roots into flat `tofu/` (one backend, one state key): move code, `tofu state mv`/pull-push between S3 keys, retire the per-project roots. `prevent_destroy` + Hetzner delete-protection on llunde-parser verified present before and after | flat root `plan` → "No changes." from a clean checkout | both pre-merge S3 state objects retained (versioned bucket) — restore either key |
| 7.2 | Docs sweep: layout references to `tofu/llunde`/`tofu/pyparser` updated; [research/phase-3-mapping.md](../../../research/phase-3-mapping.md) marked executed where facts changed | grep finds no stale paths | git revert |

## Gate

Run the exit criteria from [README.md](README.md) top to bottom; owner review closes the phase. Deviations fix the doc on the spot, per house rule.
