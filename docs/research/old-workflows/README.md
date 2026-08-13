# Old monorepo deploy workflows (reference only)

> **Historical.** Preserved copies of the pre-migration CI, kept deliberately.
> Neither file is executable here — facts, not config.

The pyparser deploy workflow did not survive the repo split (llunde-pyparser has
no `.github/`), so `old.deploy-pyparser.yml` is the authoritative record of how
its build+deploy pipeline worked: GHCR build, ssh to host, `deploy-remote.sh`
with Alembic migrate + rollback. `old.deploy-llunde.yml` documents the retired
llunde flow. Both are analysed in [current-state.md](../current-state.md); the
estate now deploys by pull, with no SSH from CI at all
([ADR 020](../../decisions/020-gitops-pull-auto-apply.md)).
