# Tailnet policy — apply runbook

`policy.hujson` is the source of truth for the Tailscale ACL. It is applied
**manually** via the admin console (login.tailscale.com/admin/acls), not via
GitOps (no Tailscale API credential in the estate — a deliberate choice).

## Why this is delicate

Defining any `acls` flips the tailnet from **default-allow to default-deny**.
If you save the restrictive `policy.hujson` while the two hosts are still
untagged, the `tag:server`/`tag:ci` rules match nothing and you instantly lose
**host↔host scraping/push and CI SSH** (management still works — your laptop is
`autogroup:member`). So tagging must happen **before** the restrictive rules
go live.

## Safe phased apply

**Phase 1 — define tags under a still-permissive policy (no lockout possible):**
Save this in the console (it defines tag ownership but restricts nothing):

```jsonc
{
  "tagOwners": { "tag:server": ["autogroup:member"], "tag:ci": ["autogroup:member"] },
  "acls": [ { "action": "accept", "src": ["*"], "dst": ["*:*"] } ]
}
```

**Phase 2 — tag the two hosts (no host command, no reauth bounce):**
Console → **Machines** → for `llunde-01` and `llunde-parser` → `⋯` →
**Edit ACL tags** → add `tag:server`. Assigning from the console is immediate
and does **not** drop the connection (unlike `tailscale up --advertise-tags`,
which silently no-ops without `--force-reauth` and bounces tailscaled).

**Phase 3 — verify under the still-permissive policy:**
- `ssh llunde-01 true` and `ssh llunde-parser true` from the laptop.
- Grafana → Prometheus targets all UP; `journalctl -u promtail` on llunde-01
  shows pushes succeeding.
- Both hosts show `tag:server` in `tailscale status`.

**Phase 4 — swap in the restrictive rules:**
Paste the full `policy.hujson` and save. The `tests` block must pass (the
console blocks a save that would break the asserted paths). Then re-verify
Phase 3's checks, plus: a `tag:ci`-tagged node cannot reach `:3000`/`:3100`.

**Revert (fast):** re-paste the Phase 1 permissive `acls` and save. Under
default-allow, tag state is irrelevant, so everything is reachable again.
Untagging a host is a *separate, reauth-gated* operation — not the quick revert.
Ultimate fallback: Hetzner web console + `hcloud server enable-rescue` (runbook §11).

## Before Phase 4 — two gates

1. **`autogroup:member` in test `src`.** If the console rejects it, swap the two
   test `src` values for your login (Users page, e.g. `you@github`).
2. **How CI joins the tailnet.** The `tag:ci` rule only isolates CI *if the
   runners advertise `tag:ci`*. If a CI runner (esp. portfolio's deploy) joins
   as your member account instead, it stays under the member rule (works, but
   unrestricted); if it joins under a *different* tag, Phase 4 **denies it and
   breaks that deploy**. Confirm portfolio's + infra CI tailnet identity before
   Phase 4, or apply Phase 4 in a window where you can watch a deploy and revert.

## Codification (reinstall-safety follow-up)

`modules/tailscale` uses NixOS autoconnect, which only runs `tailscale up` when
logged *out* — so console tags survive routine rebuilds. A **fresh reinstall**
(nixos-anywhere) would rejoin with the untagged sops key and land untagged.
To make a reinstall re-tag: rotate `secrets/tailscale.yaml` to a reusable
`tag:server`-capable key and add
`services.tailscale.extraUpFlags = [ "--advertise-tags=tag:server" ];`
to `modules/tailscale`. Not urgent (steady-state rebuilds keep the tag).
