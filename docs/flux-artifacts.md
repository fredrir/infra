# Flux artifacts

| Name | Value |
| --- | --- |
| Entrypoint | `platform/clusters/production` |
| Generated manifests | `platform/clusters/production/artifacts` |
| Controller | `platform/clusters/production/source-watcher` |
| Image | `images/source-watcher` |
| Scope | Policy and four projects, each reconciled from its own ExternalArtifact |
| Parser order | PostgreSQL readiness → migration → application |
| Policy barrier | Exact artifact name, observed generation and Ready condition, with additive dependency checks |
| Policy inputs | Policy files, shared settings and root declarations |
| Evidence | [Artifact qualification](../build/evidence/flux-artifact-qualification.json) |

```sh
go -C platform/generate run .
go -C platform/generate run . --check
go test ./internal/fluxartifacts
kubectl kustomize platform/clusters/production
```

Regenerate after changing policy inputs or project artifact components. An image-only change updates its project artifact without invalidating unrelated projects or the policy artifact.
