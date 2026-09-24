# Infrastructure reconciliation

| Document | Scope |
| --- | --- |
| [Goals](GOAL.md) | Outcomes and acceptance targets |
| [TODO](TODO.md) | Implementation, parallel local validation and rollout |

| Repository variable | Enabled behavior |
| --- | --- |
| `INFRA_SCOPE_HOSTS` | Affected host playbooks and safe deployment skips |
| `INFRA_SCOPE_PROJECTS` | Selected project deployment and verification |
| `INFRA_VERIFY_ARTIFACTS` | Exact source provenance and workload readiness |

| Local validation | Command |
| --- | --- |
| Unit and contract tests | `bazel test //...` |
| Disposable Flux cluster | `go run ./tests/infra/fluxscope` |
| Ansible container | `INFRA_ANSIBLE_TEST_IMAGE=<local-image> go test ./internal/reconcile -run TestAnsibleScopeWithLocalContainers` |

Scope controls default to disabled; project scoping requires artifact verification, and disabling scope restores full execution for deployment changes.
