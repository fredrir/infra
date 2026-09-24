# Infrastructure reconciliation

| Document | Scope |
| --- | --- |
| [Goals](GOAL.md) | Outcomes and acceptance targets |
| [TODO](TODO.md) | Implementation, parallel local validation and rollout |
| [Evidence](../../../build/evidence/infra-reconcile.json) | Local tests, independent review and production results |

| Repository variable | Enabled behavior | Production |
| --- | --- | --- |
| `INFRA_SCOPE_HOSTS` | Affected playbooks and safe deployment skips | Enabled |
| `INFRA_SCOPE_PROJECTS` | Selected project deployment and verification | Enabled |
| `INFRA_VERIFY_ARTIFACTS` | Exact provenance and workload readiness | Enabled |

| Local validation | Command |
| --- | --- |
| Unit and contract tests | `bazel test //...` |
| Disposable Flux cluster | `go run ./tests/infra/fluxscope` |
| Ansible container | `INFRA_ANSIBLE_TEST_IMAGE=<local-image> go test ./internal/reconcile -run TestAnsibleScopeWithLocalContainers` |

CLI flags default to disabled; project scoping requires a verified artifact baseline, and disabling scope restores full execution for deployment changes.
