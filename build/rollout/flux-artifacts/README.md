# Selective Flux reconciliation

| Name | Value |
| --- | --- |
| State | Optional; absent from the active production resource list |
| Approval | Required before selecting an overlay in production |
| Flux | 2.9.5 |
| Extension | source-watcher 2.2.4, immutable image digest in `controller/source-watcher.yaml` |
| Image qualification | Blocked until the downstream candidate passes scanning and provenance verification; the upstream pin has four fixable HIGH findings in `image/upstream-scan.json` |
| Added resources | One CRD, deployment, service, service account, namespaced role and binding |
| CPU | 50m requested, 1 core limit |
| Memory | 64Mi requested, 256Mi limit; requires qualification |
| Temporary storage | Two emptyDir volumes, 512Mi limit each |
| Permissions | Flux source reads, generated artifacts/status, leader election and events in `flux-system` |
| Secret access | None for the new controller; existing SOPS reconciliation remains unchanged |
| Scope | Policy plus four projects; parser migration still precedes parser application |
| Latency | Unmeasured; no sub-minute claim |

The generator separates project inputs into content-addressed ExternalArtifacts, so an image-only commit does not invalidate the policy artifact or unrelated projects. Shared settings, backup jobs and their repository-maintenance component remain declared inputs. Existing non-project platform components still follow the GitRepository revision.

The policy artifact name includes a content hash of policy files, shared settings and root reconciliation declarations. Every policy dependency retains the built-in check and additionally requires that exact source name, observed generation and Ready condition; a simultaneous policy/application change cannot accept the old Ready status or the old artifact. Regenerate after editing these inputs. A stale generated barrier must block activation.

```sh
go -C build/rollout/flux-artifacts/generate run .
go -C build/rollout/flux-artifacts/generate run . --check
go -C build/rollout/flux-artifacts/generate test ./...
kubectl kustomize build/rollout/flux-artifacts/bootstrap >/tmp/flux-bootstrap.yaml
kubectl kustomize build/rollout/flux-artifacts/pause >/tmp/flux-pause.yaml
kubectl kustomize build/rollout/flux-artifacts/cutover >/tmp/flux-cutover.yaml
```

## Approved activation

Commit and push the reviewed overlays before selecting a path. Pause image promotions during ownership handoff. Stop if any check fails; leave the current workloads running.

| Phase | Effect | Required evidence |
| --- | --- | --- |
| Bootstrap | Install the extension and create artifacts; current application ownership remains intact | Controller Available, generator Ready, all five artifacts present |
| Pause | Suspend the aggregate project owner with pruning disabled; move policy to the versioned artifact | Aggregate not reconciling, policy Ready at its observed generation |
| Cutover | Create four project owners and redirect parser child sources | Every project Ready, workload inventory labels moved, parser migration/application Ready |

```sh
kubectl -n flux-system patch kustomization flux-system --type=merge -p '{"spec":{"path":"./build/rollout/flux-artifacts/bootstrap"}}'
flux reconcile kustomization flux-system --with-source --timeout=30s
kubectl -n flux-system rollout status deployment/source-watcher --timeout=30s
kubectl -n flux-system wait artifactgenerator/platform-artifacts --for=condition=Ready --timeout=30s
kubectl -n flux-system get externalartifacts
kubectl -n flux-system auth can-i get secrets --as=system:serviceaccount:flux-system:source-watcher-artifacts
```

The last command must print `no`.

```sh
kubectl -n flux-system patch kustomization flux-system --type=merge -p '{"spec":{"path":"./build/rollout/flux-artifacts/pause"}}'
flux reconcile kustomization flux-system --timeout=30s
kubectl -n flux-system get kustomization platform-projects -o json | jq -e '.spec.suspend == true and .spec.prune == false and .spec.deletionPolicy == "Orphan" and ([.status.conditions[]? | select(.type == "Reconciling" and .status == "True")] | length) == 0'
kubectl -n flux-system wait kustomization/platform-policy --for=condition=Ready --timeout=30s
kubectl -n flux-system get kustomization platform-policy -o json | jq -e '.metadata.generation == .status.observedGeneration and .spec.sourceRef.kind == "ExternalArtifact"'
```

```sh
kubectl -n flux-system patch kustomization flux-system --type=merge -p '{"spec":{"path":"./build/rollout/flux-artifacts/cutover"}}'
flux reconcile kustomization flux-system --timeout=30s
for project in llunde portfolio y llunde-pyparser; do
  kubectl -n flux-system wait "kustomization/project-$project" --for=condition=Ready --timeout=30s
done
kubectl -n flux-system wait kustomization/llunde-pyparser-migration kustomization/llunde-pyparser-application --for=condition=Ready --timeout=30s
kubectl -n llunde get deployment web -o json | jq -e '.metadata.labels["kustomize.toolkit.fluxcd.io/name"] == "project-llunde"'
```

Keep the old `platform-projects` inventory suspended with `prune: false` and `deletionPolicy: Orphan` during qualification. Check resource identities against the pre-migration inventory before retiring that owner. Confirm an image-only commit changes only the matching project artifact, preserves policy revision/readiness, and serves the expected application revision. Compare controller requests, reconciliation latency and application health before enabling regular promotions.

## Rollback

Disable pruning on the new owners before removing their declarations. Restore the bootstrap path first; it restores the aggregate owner and original parser/policy Git sources while leaving the extension available to finalize its objects.

```sh
for project in llunde portfolio y llunde-pyparser; do
  kubectl -n flux-system patch "kustomization/project-$project" --type=merge -p '{"spec":{"suspend":true,"prune":false,"deletionPolicy":"Orphan"}}'
done
kubectl -n flux-system patch kustomization flux-system --type=merge -p '{"spec":{"path":"./build/rollout/flux-artifacts/bootstrap"}}'
flux reconcile kustomization flux-system --timeout=30s
flux reconcile kustomization platform-projects --timeout=30s
kubectl -n flux-system wait kustomization/platform-projects --for=condition=Ready --timeout=30s
```

After checking aggregate ownership and application health, remove the generator while its controller is still running, then restore the original production path.

```sh
flux suspend kustomization flux-system
kubectl -n flux-system delete artifactgenerator platform-artifacts --wait=true --timeout=30s
kubectl -n flux-system patch kustomization flux-system --type=merge -p '{"spec":{"path":"./platform/clusters/production"}}'
flux resume kustomization flux-system
flux reconcile kustomization flux-system --timeout=30s
```

## Sources

- [Official ArtifactGenerator content revisions and copy semantics](https://fluxcd.io/flux/components/source/artifactgenerators/)
- [Flux dependency readiness expressions](https://fluxcd.io/flux/components/kustomize/kustomizations/#dependency-ready-expression)
- [Flux pruning and deletion policy](https://fluxcd.io/flux/components/kustomize/kustomizations/#deletion-policy)

The extension manifest derives from `flux install --version=v2.9.5 --components-extra=source-watcher --export`; namespace scope, permissions, image digest and resource bounds are explicit local restrictions. The new controller and ownership transfer require independent review and production qualification.
