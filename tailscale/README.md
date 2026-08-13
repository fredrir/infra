# Tailnet policy — apply reference

`policy.hujson` is the source of truth for the Tailscale ACL, applied **by hand**
in the admin console (login.tailscale.com/admin/acls), not by GitOps: the estate
deliberately holds no Tailscale API credential.

## Why this is delicate

Defining any `acls` flips the tailnet from **default-allow to default-deny**.
Saving the restrictive `policy.hujson` while the two hosts are untagged leaves
the `tag:server`/`tag:ci` rules matching nothing and instantly kills **host↔host
scraping/push and CI SSH** (management survives — the laptop is
`autogroup:member`). Tagging happens **before** the restrictive rules go live.

## Safe phased apply

**Step 1 — define tags under a still-permissive policy** (no lockout possible).
Save this; it defines tag ownership and restricts nothing:

```jsonc
{
  "tagOwners": { "tag:server": ["autogroup:member"], "tag:ci": ["autogroup:member"] },
  "acls": [ { "action": "accept", "src": ["*"], "dst": ["*:*"] } ],
  // The acls do NOT govern Tailscale SSH: without this block, `ssh archie`
  // breaks the moment any custom policy is saved.
  "ssh": [ { "action": "check", "src": ["autogroup:member"], "dst": ["autogroup:self"], "users": ["autogroup:nonroot", "root"] } ]
}
```

**Step 2 — tag the hosts** (no host command, no reauth bounce). Console →
**Machines** → `llunde-01` and `llunde-parser` → `⋯` → **Edit ACL tags** → add
`tag:server`. Console assignment is immediate and does **not** drop the
connection, unlike `tailscale up --advertise-tags`, which silently no-ops
without `--force-reauth` and bounces tailscaled.

**Step 3 — verify while still permissive:** `ssh llunde-01 true` and `ssh
llunde-parser true` from the laptop; Grafana shows every Prometheus target UP
and `journalctl -u promtail` on llunde-01 shows pushes succeeding; both hosts
show `tag:server` in `tailscale status`.

**Step 4 — swap in the restrictive rules.** Paste `policy.hujson` and save; the
`tests` block must pass (the console blocks a save that breaks an asserted
path). Re-run step 3, plus: a `tag:ci` node cannot reach `:3000`/`:3100`.

**Revert (fast):** re-paste the step-1 permissive `acls`. Under default-allow
tag state is irrelevant, so everything is reachable again. Untagging a host is a
*separate, reauth-gated* operation, not the quick revert. Ultimate fallback:
Hetzner web console + `hcloud server enable-rescue` (runbook §11).

## Narrowing rule 1 takes two pastes

Rule 1 is the personal mesh; estate reach belongs to the two management devices
(`hosts` aliases macie/archie, by node IP — deliberately not a tag; see the
comment in `policy.hujson`). Any narrowing of it goes in TWO pastes, so it is
never live before the management rule is proven: first the current policy PLUS
the `hosts` block and management rule, rule 1 still `member → *:*` and the old
tests — verify from the laptop that SSH to both hosts, Grafana `:3000` and
Prometheus `:9090` work; then `policy.hujson` verbatim, re-verifying those paths
**and `ssh archie`** (the mesh cannot be asserted in the tests block — the
framework cannot evaluate autogroup dsts, tailscale/tailscale#4416 — so that
live check IS the mesh proof). Finish on the deny side: from the PHONE (a
generic member device) Grafana and `:9090` must be unreachable, and a portfolio
deploy must still work, its runner being `tag:ci`.

**Revert:** re-paste the pre-narrowing policy from this file's git history. If a
management device is reinstalled or rejoins as a new node, update its IP in
`hosts` and re-paste; until then it has no estate reach — the other device, or
the Hetzner console, is the way in.

## Mechanics that bite

1. **Autogroups are rejected in a test `src`** — both `autogroup:owner` and
   `autogroup:member`. The management test uses the concrete login
   `fredrir@github`; `autogroup:member` stays valid inside `acls`.
2. **CI identity.** Both CI auth keys (`ci-portfolio`, `ci-pyparser`) carry
   `tag:ci` and the infra repo's CI never joins the tailnet, so rule 3 covers
   portfolio deploys AND isolates them — but only *while* the runners advertise
   the tag; they do.
3. **Tailscale SSH needs its own `ssh` block.** Replacing the default policy
   drops Tailscale's built-in self-SSH rule and breaks `ssh archie` (any
   `--ssh`-enabled device). Both `policy.hujson` and the step-1 snippet carry
   the block; the estate hosts run regular sshd and are unaffected.
4. **A stale tag strands a device — the tests are the smoke alarm.** Tagged
   devices have no user, so ownership-based dsts
   (`autogroup:member`/`autogroup:self`) can NEVER match them; that only worked
   while rule 1's dst was `*:*`. Symptoms: absent from every peer's netmap,
   MagicDNS NXDOMAIN, tests reporting "want: Accept, got: Drop" against its IP.
   The test engine resolves dst ownership and is right — read the console's
   machine page ("Managed by tag:… — tag was deleted from the policy file, never
   removed from this machine") before theorizing.

   **Remote rescue (proven):** the console cannot remove a machine's LAST tag,
   only reauth can. (1) Temp-paste three additions — the stale tag in
   `tagOwners`, `{src: [<mgmt device>], dst: ["<stranded-IP>:*"]}`, and an ssh
   rule `{action: accept, src: [autogroup:member], dst: [<the tag>], users:
   [autogroup:nonroot, root]}`; a tagged dst never matches `autogroup:self`, and
   `accept` rather than `check` avoids a browser loop mid-rescue. (2) ssh in and
   run `tailscale up --force-reauth`; it errors with the exact flag set to
   restate (e.g. `--ssh --hostname=…`). (3) Open the printed auth URL anywhere
   and authenticate as the owner: that user becomes the manager, which strips
   the tag; the node keeps its IP and the SSH session survives. (4) Approve
   under Device Approval if held. (5) Re-paste the final policy to unwind the
   temp blocks; verify the mesh and the estate paths.

## Reinstall safety

`modules/tailscale` uses NixOS autoconnect, which runs `tailscale up` only when
logged *out*, so console tags survive routine rebuilds. A **fresh reinstall**
(nixos-anywhere) rejoins with the untagged sops key and lands untagged. To make
a reinstall re-tag: rotate `secrets/tailscale.yaml` to a reusable
`tag:server`-capable key and add
`services.tailscale.extraUpFlags = [ "--advertise-tags=tag:server" ];` to
`modules/tailscale`. Not urgent — steady-state rebuilds keep the tag.
