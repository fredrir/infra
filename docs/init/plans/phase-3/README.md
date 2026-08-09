# Phase 3 — pyparser groundwork, one management plane (top-level plan)

**Rescoped 2026-08-09** after the phase-2 gate closed and the llunde-pyparser + portfolio mapping landed ([research/phase-3-mapping.md](../../../research/phase-3-mapping.md)). Two facts moved the boundaries:

1. **portfolio is already live on `llunde-parser`** (hansteen.dev) as a self-managed rootless-podman tenant — so NixOS adoption of that box is a *two*-live-tenant migration, not one. It moves to its own phase: [phase 3.5](../phase-3.5/README.md), the final push to full NixOS ([ADR 016](../../../decisions/016-tenant-slots-host-uniformity.md)).
2. **llunde-pyparser has no CI at all** — the deploy workflow died with the old monorepo. Until it is recreated, a pyparser change can only reach prod by hand-run SSH. The old workflow was push-to-main → CI builds → CI runs the orchestrated deploy over SSH; that is what gets recreated, tailnet-native from day one.

Phase 3 is therefore everything that de-risks 3.5 without touching how pyparser *runs*: state off the laptop, DNS into code, CI restored, one management plane ([ADR 015](../../../decisions/015-tailnet-only-management.md)), flat tofu. pyparser's containers, compose file, and data are not modified.

## In scope (ordered by risk, each step independently valuable)

- **State → S3**: `tofu/pyparser` migrates to the S3 backend (`tofu init -migrate-state` — the stub already sits commented in its `versions.tf`). Proof: steady-state `plan` reports **"No changes."** before and after ([ADR 002](../../../decisions/002-opentofu-s3-state.md)).
- **Cloudflare zone into `tofu/modules/cloudflare/`** ([ADR 012](../../../decisions/012-cloudflare-strategy.md), owner-confirmed for this phase): import the `llunde.no` zone records — grey-cloud A/AAAA for `llunde.no`/`www`/`api`, the pyparser tunnel CNAMEs (`parser`, `external`) — **records only; tunnels and Access apps stay out of tofu** (token-embeds-secret trap, `tofu/pyparser/CLOUDFLARE.md`). Side effect: verify the retired llunde tunnel is actually deleted. `hansteen.dev` is portfolio's own zone and is not managed here.
- **Tailscale on `llunde-parser`**: join the tailnet (an apt package + auth key — touches neither tenant's stack), prove root SSH over it, repoint the `letzner` alias and the `pyparser-sync` laptop tunnel.
- **pyparser CI recreation** (in the llunde-pyparser repo): faithful port of `old.deploy-pyparser.yml` — same jobs, `image_tag` dispatch lever, keep-10 GHCR prune — adjusted for repo-root context and reaching the host **over the tailnet**. The bespoke image build stays (torch + docling model layers want GHA cache); unification with `build-image.yml` belongs to 3.5.
- **portfolio deploys over the tailnet**: a small PR to the portfolio repo — a Tailscale step in its deploy job and `DEPLOY_HOST` flipped to the tailnet name. Nothing else about portfolio changes; it stays ignorant of llunde ([ADR 016](../../../decisions/016-tenant-slots-host-uniformity.md)).
- **Close port 22 everywhere** ([ADR 015](../../../decisions/015-tailnet-only-management.md)): after every SSH consumer is proven on the tailnet — laptop, pyparser CI, portfolio CI — drop 22 from both Hetzner firewalls, `llunde-01`'s NixOS firewall, and `llunde-parser`'s ufw. Break-glass becomes the Hetzner console/rescue system plus a documented tofu re-open.
- **Tofu unnesting**: with both states in S3, merge the per-project roots into flat `tofu/*` — accepting the recorded one-blast-radius consequence; `prevent_destroy` + Hetzner delete-protection stay on `llunde-parser` throughout ([ADR 002](../../../decisions/002-opentofu-s3-state.md)).

## Out of scope

Any change to pyparser's runtime (compose stack, image contents, alembic placement) — 3.5. NixOS on `llunde-parser` — 3.5. Backups restructuring — pyparser's nightly S3 ship already satisfies the intent of [ADR 011](../../../decisions/011-backups-restic-module.md) and is documented, not rebuilt, while the host is still docker-compose. Orange-cloud / tunnel-vs-proxy — phase 4 opens with that discussion, per the owner.

## Hard rules

1. One change at a time, each with its own rollback note ([tasks.md](tasks.md)).
2. `parser.llunde.no` **and** `hansteen.dev` availability are gate metrics for every step — two live prods share the box.
3. Port 22 closes only after every consumer has a proven tailnet path; never close and migrate in the same step.

## Exit criteria (gate)

1. Both tofu roots (then the flat root) plan **"No changes."** from a fresh checkout with no local state.
2. `llunde.no` zone records are tofu-managed; `tofu plan` clean; `dig` answers unchanged; the old llunde tunnel confirmed gone.
3. A pyparser push-to-main deploys through the recreated workflow over the tailnet (and an `image_tag` dispatch re-deploy — the rollback lever — is exercised once).
4. A portfolio deploy completes over the tailnet; its synthetic checks stay green.
5. Public port 22 refused on both boxes; SSH over tailnet works for laptop and both CI paths; break-glass procedure documented and rehearsed to the point of a rescue-console login.
6. `parser.llunde.no` and `hansteen.dev` never degraded during the phase.
7. Owner review → gate closed; phase 3.5 planning may start.
