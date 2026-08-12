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
  "acls": [ { "action": "accept", "src": ["*"], "dst": ["*:*"] } ],
  // Keep Tailscale SSH to your own --ssh devices working — the acls do NOT
  // govern Tailscale SSH; without this, `ssh archie` breaks the moment any
  // custom policy is saved.
  "ssh": [ { "action": "check", "src": ["autogroup:member"], "dst": ["autogroup:self"], "users": ["autogroup:nonroot", "root"] } ]
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

## Management scoping (second apply, 2026-08-11) — two-step order

The member rule was narrowed from `*:*` to the personal mesh, with estate
reach moved to the two management devices (`hosts` aliases macie/archie, by
node IP — deliberately not a tag; see the comment in policy.hujson). Applying
it is TWO pastes, so there is never a moment where the narrowing is live
before the management rule is proven:

1. **Additive paste**: current policy PLUS the `hosts` block and the
   management rule, with rule 1 still `member → *:*` and the OLD tests.
   Verify from the laptop: SSH both hosts, Grafana `:3000`, Prometheus
   `:9090` all work (they now match the management rule first).
2. **Final paste**: this repo's `policy.hujson` verbatim (rule 1 narrowed,
   new tests). Re-verify the laptop paths **and `ssh archie`** (the personal
   mesh cannot be asserted in the tests block — the test framework cannot
   evaluate autogroup dsts, tailscale/tailscale#4416 — so this live check IS
   the mesh proof). Then the deny side: from the PHONE (a generic member
   device), Grafana/`:9090` must now be unreachable.
   Belt-and-suspenders: trigger a portfolio deploy (its runner is `tag:ci` —
   gotcha 2 below — so rule 3 covers it; this proves that stayed true).

**Revert**: re-paste the pre-narrowing policy (git history of this file).
If a management device is reinstalled/rejoins as a new node, update its IP
in `hosts` and re-paste — until then that device has no estate reach (the
other device, or the Hetzner console, is the path in the meantime).

## Gotchas hit on the first apply (resolved — kept as lessons)

1. **Autogroups are rejected in a test `src`** — confirmed for both
   `autogroup:owner` and `autogroup:member`. The management test uses the
   concrete login `fredrir@github`; the `acls` keep `autogroup:member` (valid
   there).
2. **CI identity confirmed.** Both CI auth keys (`ci-portfolio`, `ci-pyparser`)
   carry `tag:ci`, and the infra repo's CI never joins the tailnet — so rule 3
   covers portfolio deploys AND isolates them. (`tag:ci` only isolates CI *if*
   the runners advertise it; they do.)
3. **Tailscale SSH needs its own `ssh` block.** Replacing the default policy
   dropped Tailscale's built-in self-SSH rule, which broke `ssh archie` (a
   `--ssh`-enabled device). The estate hosts run regular sshd (unaffected). The
   policy now carries the self-SSH block, and so does the Phase-1 snippet above.

## Codification (reinstall-safety follow-up)

`modules/tailscale` uses NixOS autoconnect, which only runs `tailscale up` when
logged *out* — so console tags survive routine rebuilds. A **fresh reinstall**
(nixos-anywhere) would rejoin with the untagged sops key and land untagged.
To make a reinstall re-tag: rotate `secrets/tailscale.yaml` to a reusable
`tag:server`-capable key and add
`services.tailscale.extraUpFlags = [ "--advertise-tags=tag:server" ];`
to `modules/tailscale`. Not urgent (steady-state rebuilds keep the tag).
