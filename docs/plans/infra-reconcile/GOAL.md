# Goals

| What | Target | Status |
| --- | --- | --- |
| Host scope | Run and verify only affected playbooks and their prerequisites | Not done |
| Project scope | Deploy through existing Flux owners; verify only the selected project and dependencies | Not done |
| Deployment correctness | Prove artifact freshness, workload readiness and parser migration ordering | Not done |
| Safe state | Failures never advance Applied; skipped deployments preserve the deployed baseline | Not done |
| Recovery | Unknown inputs select full mode; scoped checkpoints remain safe across CLI rollback | Not done |
| Efficient validation | Parallel isolated local tests, reused fixtures/caches and focused checks for environment gaps | Not done |
| Rollout | Permissions precede scoped execution; full recovery, production locking and ownership remain intact | Not done |
| Performance evidence | Capture timings during required tests and normal runs without observation periods or completion estimates | Not done |
