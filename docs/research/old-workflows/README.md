# Old monorepo deploy workflows (reference only)

Re-added by the owner as phase-3 reference material: the pyparser deploy
workflow did not survive the repo split (llunde-pyparser has no `.github/`),
and `old.deploy-pyparser.yml` is the authoritative record of how its
build+deploy pipeline worked (GHCR build, ssh to host, `deploy-remote.sh`
with Alembic migrate + rollback). `old.deploy-llunde.yml` documents the
retired llunde flow. Neither is executable here — facts, not config.
