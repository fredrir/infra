# Phase 3.5 — task breakdown & delegation

Same model as phase 2: **declarative files in parallel streams; every live surface serial by the lead.** Streams verify with `nix flake check` + `nixos-rebuild build --flake .#llunde-parser`. Two live productions ride on the serial part — the rehearsal is not skippable.

## Step 0 — Contract & prerequisites (lead)

| # | Task | Detail |
|---|---|---|
| 0.1 | [contract.md](contract.md) review | uids, units, knob mapping, secrets — frozen before streams start |
| 0.2 | **Host identity pre-generated NOW** (not at cutover — the rehearsal needs it) | `ssh-keygen` the llunde-parser host key, keep at `/tmp/llunde-parser-keys/`; `.sops.yaml` gains per-host creation rules + the new host recipient; `sops updatekeys` re-encrypts the shared files (`tailscale.yaml`, `ghcr.yaml`) |
| 0.3 | Secrets minted | Fresh Doppler `pyparser/prd` host token; fresh tailscale auth key; restic password; verify `TUNNEL_TOKEN`'s exact key name against live Doppler `prd` (it appears in no repo) — sops files committed encrypted |
| 0.4 | `flake.nix` gains `llunde-parser` | Host skeleton evaluating empty, so every stream builds against it |

## Step 1 — Parallel streams (sub-agents; flake-build green each)

| Stream | Owns | Builds | Done when |
|---|---|---|---|
| **A — Host** | `hosts/llunde-parser/`, `modules/profiles/` param, `modules/secrets/` param | disko (UEFI/systemd-boot, `/dev/sda`), qemu-guest import, `allowedTCPPorts` per-host option (**this host: none**), tailscale, **per-host secrets parameterization** (llunde-01's five stay; llunde-parser's four per contract) | closure builds for BOTH hosts; zero-open-ports asserted in a golden check |
| **B — pyparser stack** | `services/pyparser/` | the seven units per the contract table (postgres, **migrate oneshot**, review, both workers, cloudflared) + network + env-render oneshot (`/run/pyparser/secrets.env`) **ordered by the system manager before `user@2001.service`**; `EnvironmentFile=` on every unit; auto-update labels per contract (apps only) | closure builds; rendered unit text reviewed against the compose file side-by-side |
| **C — Tenant slot** | `modules/tenants/` | generic slot module (user, uid, subuids, linger, packages, no content) + `portfolio.nix` instance per contract | closure builds; slot golden check (subuid file, linger, packages on PATH) |
| **D — Backups & docs** | `modules/backups/` adoption, `docs/pyparser/PROD.md`, `docs/runbook.md` | restic job (pg_dump preHook + files volume, weekly, prefix `restic/llunde-parser`); PROD.md rewritten for the NixOS world (restic restore, journalctl-not-Dozzle, no SSH deploys); runbook extended to two hosts | docs reviewed; timer/unit text reviewed |
| **E — pyparser CI** | llunde-pyparser `.github/` | `deploy.yml` → `build.yml` (build+push+prune only) staged on a branch, **merged only at cutover** (until then SSH-deploy keeps working) | PR open, unmerged |

## Step 2 — Rehearsal (lead, serial; disposable Hetzner box, hourly billing)

| # | Task | Proof | Rollback |
|---|---|---|---|
| 2.1 | Create scratch server (cx-class, UEFI), nixos-anywhere the `llunde-parser` config onto it with the step-0.2 host key. **Two identity guards from first boot: cloudflared units masked** (a second connector would take real traffic) **and no production tailnet identity** (tailscale disabled or hostname overridden — the scratch box must never join as `llunde-parser` while prod lives) | boots, sops decrypts, zero public ports, absent from `tailscale status` under the prod name | delete server |
| 2.2 | Restore drill: yesterday's pg_dump + files tar → new units; full stack up | in-box `curl review:8081/healthz` OK; workers idle-clean in journal; migrate unit ran alembic to head | n/a (scratch) |
| 2.3 | Slot drill: portfolio slot present; as `portfolio`, drop a dummy `.container` into `~/.config/containers/systemd/`, `systemctl --user daemon-reload`, unit generates and runs | dummy container active | n/a |
| 2.4 | Auto-update drill: retag a test image, timer pulls + restarts with migrate ordering respected | journal shows migrate→app order | n/a |
| 2.5 | Delete scratch box; fold every deviation into code/docs (fix-the-doc) | diff review | — |

## Step 3 — Cutover (lead, serial; announced maintenance window)

| # | Task | Proof / gate metric | Rollback |
|---|---|---|---|
| 3.1 | Freeze: announce window; verify no CI runs in flight; portfolio: force fresh `backup.sh` + `wal-ship`; pyparser: final `pg_dump -Fc` + files tar → laptop **and** S3 | dumps verified restorable (spot `pg_restore --list`) | abort — nothing touched |
| 3.2 | Final config sanity: `nixos-rebuild build` green, secrets decrypt with the step-0.2 identity (already committed) | build log | abort |
| 3.3 | 💥 nixos-anywhere wipes the box (`--extra-files` host key; kexec; disko). **Immediately after the wipe, before the new install's `tailscale up`: delete the old `llunde-parser` node in the admin console** — the stale non-ephemeral record would otherwise hold the MagicDNS name and force `llunde-parser-1` | first boot: sops decrypts, tailscale joins as **exactly** `llunde-parser` (verify FQDN resolves to the new node), zero public ports | Hetzner rescue + restore-from-backups (both tenants' data is off-box) |
| 3.4 | Restore pyparser: dump → postgres unit, tar → files volume; start stack; **unmask cloudflared last** | tunnel reconnects; `parser.llunde.no` 302→app, Access login works; **`external.llunde.no` `/media/share` guard behavior intact**; queue depth sane | stop stack, re-mask tunnel, investigate — old dumps remain authoritative |
| 3.5 | portfolio: slot ready → owner/lead runs its `install.sh` (**pass the host arg — its default is the retired `letzner` alias**) + DB restore + `deploy <sha>` per **its** runbooks (llunde-infra hands off at the slot boundary); re-scan `DEPLOY_KNOWN_HOSTS` for the new host key and set it **with `-e Production`** (environment secrets shadow repo secrets) | `hansteen.dev` 200 + `/api/v1/version` correct; a real CI deploy green post-cutover; its synthetic checks green | portfolio's own rollback/DR runbooks |
| 3.6 | Merge stream-E PR (CI → build-only); push a trivial pyparser change | **auto-update proof**: box swaps digest, migrate ran, zero SSH | dispatch old workflow is gone — pin previous digest via `Image=` instead (that IS the rollback lever now) |
| 3.7 | Digest-pin drill: pin `Image=` to the previous digest, rebuild, verify, unpin | old digest runs, then new again | — |
| 3.8 | Backups live: force one restic run (pg_dump preHook + files); restore-verify on the box; confirm portfolio timers all green | snapshot in `restic/llunde-parser`; `pg_restore` validates | — |
| 3.9 | Reboot test (**explicitly proves the env-render-before-user-manager ordering** — the stack must come up with populated env, unattended); `ss -tlnp` shows nothing public; retire Doppler `pyparser/ci` deploy secrets + the `DOPPLER_TOKEN_PYPARSER_CI` repo secret; update memory-facts docs (`current-state` note, phase-3-mapping executed-marks) | all green | — |

## Gate

[README](README.md) exit criteria walked top-to-bottom; owner review closes the phase — and with it, the estate is 100 % NixOS. Phase 4 opens with the tunnel-vs-orange-cloud discussion, as the owner set long ago.
