# TODO

| What | Status |
| --- | --- |
| Classify tracked inputs, deletions and mixed changes; default unknown inputs and incomplete recovery to full mode | Not done |
| Render the active cutover, project artifacts and both parser children; enforce generator `--check` in CI | Not done |
| Persist host scope with separate subset stage keys; reject unsafe reuse in old and new CLI versions | Not done |
| Audit playbook prerequisites and runtime inputs; implement full, runner-only, monitoring-only and no-host execution with matching verification | Not done |
| Prove source-code skip exceptions; report evaluated revisions without publishing or advancing Desired/Applied | Not done |
| Add minimal artifact/workload read permissions; regenerate policy barriers and preflight access | Not done |
| Verify artifact freshness in full and scoped modes, including unchanged content and generator lag | Not done |
| Carry project selection through apply/verify using existing owners; check workload images/readiness, Helm dependencies and parser ordering | Not done |
| Report scope, selection reasons, last full verification and queue/stage timings during existing runs | Not done |
| Parallel local tests: selection, state failures, checkpoint compatibility, cancellation, locking and full recovery | Not done |
| Parallel container checks: generated overlays, artifact rendering, parser coverage and malformed topology | Not done |
| Parallel local Flux tests: pinned controllers, stale artifacts, unavailable workloads, selected/unrelated failures, RBAC and request tokens | Not done |
| Parallel Ansible simulations: equivalent full/scoped outcomes, idempotence and partial failure; focused VM checks for container gaps | Not done |
| Reuse fixtures/caches, isolate mutable state and cap concurrency; rerun affected checks and retain pass/fail evidence | Not done |
| Pass local gates, deploy permissions through full reconciliation, then enable separate host/project scope controls | Not done |
| Smoke-test a simple project, llunde Helm and parser; confirm full convergence and disabling scope restores full execution | Not done |
