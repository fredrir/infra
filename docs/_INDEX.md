# Docs

## Layout

| Path                                        | Description                                           |
| ------------------------------------------- | ----------------------------------------------------- |
| [`research`](research/)                     | Important research on unkown / new areas/technologies |
| [`plans.md`](plans/README.md)               |                                                       |
| [`plans/phase-x/`](plans/phase-x/README.md) | A long term plan                                      |




## Execution model

Same model that built the backend: each phase runs as its own working session ending in a **gate** the owner reviews before the next phase starts.

1. **Foundation step (lead, sequential)** — the serial part: skeletons, contracts, anything later streams compile/evaluate against.
2. **Parallel streams (sub-agents)** — independent workstreams cut along directory boundaries so agents never touch the same files. Each stream brief names the ADRs it must follow and its done-criteria; sub-agents never make new architectural decisions — anything uncovered goes back to the lead.
3. **Integration + gate (lead)** — the lead merges, applies (all host mutations are lead-only), runs the gate checklist, and presents results.

Infra-specific rule: **declarative files can be authored in parallel; the live host is only ever touched serially by the lead.** `tofu plan` / `nix flake check` are the compile-equivalents every stream must leave green.

## Cross-repo touchpoints

- **llunde-backend**: its `docs/base/phase-3` shrinks to Containerfile + CI caller (amended in phase 1); quadlet/runbook/proxy live here ([ADR 014](../decisions/014-scope-boundaries.md)).
- **llunde-frontend** (freshly split): needs a Containerfile + CI caller in phase 2 ([ADR 009](../decisions/009-frontend-as-container.md)).
- **openclaw**: wiped with the box, not redeployed by this repo; user slot reserved.
