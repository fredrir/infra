# Infrastructure rollout

| Stage | Requirement | Repository action |
| --- | --- | --- |
| Source | Go, Bazel, native policy and workflow checks pass | Merge the reviewed source revision |
| CLI release | Protected `infra-v*` tags; tag commit belongs to `main` | Apply [tag rules](../.github/cli-tags-ruleset.json), then publish an `infra-v*` tag |
| Binary trust | Release attestation, source revision and manifest SHA-256 agree | Install the exact platform artifact; retain the previous binary |
| Images | Published digest, scan and provenance; compiled CLI included | Promote tools and runner image pins |
| External callers | Exact reviewed workflow SHA; matching OctoSTS policy; native test commands | Apply each repository patch from `build/rollout` after its expected blobs match |
| VM engines | Dedicated trusted VM, native qualification and registered runner | Apply `ansible/build-engines.yml`; enable the qualified pool explicitly |
| Retirement | All callers moved; old jobs drained; rollback data retained | Remove legacy runner and Attic service resources |

## Tools and hosts

```sh
infra platform promote-tools --image "$VERIFIED_TOOLS_IMAGE"
infra platform promote-tools --image "$VERIFIED_TOOLS_IMAGE" --apply
git diff -- platform

ANSIBLE_CONFIG=ansible/ansible.cfg ansible-playbook \
  -i "$HOST_INVENTORY" ansible/infra-cli.yml \
  -e "infra_binary_url=$INFRA_BINARY_URL" \
  -e "infra_binary_revision=$INFRA_REVISION" \
  -e "infra_binary_sha256=$INFRA_SHA256"
```

`VERIFIED_TOOLS_IMAGE` must be a published `ghcr.io/fredrir/platform-backup-tools@sha256:...` reference. Local OCI exports demonstrate build behavior; their digests are not published deployment artifacts.

## Legacy retirement

| Resource | Cutover condition | Retention |
| --- | --- | --- |
| BuildKit ARC pools and cache credentials | Every image and package caller uses the hosted Dagger workflow; old jobs drained | Previous workflow/image pins for rollback |
| Nix ARC pool and Attic reader credentials | Infrastructure checks use Bazel; no remaining Nix callers | Previous workflow/image pins for rollback |
| Attic Deployment, Service and tunnel | No consumers; replacement path verified | Attic namespace, PVC, local data and backups until rollback window closes |
| Attic monitoring | Service retirement approved | Re-encrypt Gatus configuration with SOPS; keep its integrity metadata valid |
| Attic provider resources | Reviewed OpenTofu plan | Preserve backup objects; resolve `prevent_destroy` explicitly |
| Legacy platform scripts | Published Go tools image promoted with commands | Remove their ConfigMap generators and source files in the same change |

The current Flux cache Kustomization uses `prune: true`; removing it or the namespace without a retention change can delete persistent claims. Retain data before pruning service resources.

## Evidence

| Check | Evidence |
| --- | --- |
| Independent Bazel cache reuse | [Cold and warm action-cache experiment](../build/evidence/bazel-cache-linux-amd64.json) |
| Native Linux quality | [Race tests and vet at the recorded revision](../build/evidence/go-validation-linux-amd64.json) |
| External source state | [Consumer inventory](../build/consumers.json); refresh before applying patches |

| Deferred validation | Gate |
| --- | --- |
| Full Kata/QEMU/kernel rebuild | Dedicated engine and explicit time budget |
| Native ARM execution | Qualified ARM host |
| Production backup recovery and runtime activation | Approved deployment window and restore evidence |
| VM pool activation | Qualified dedicated VM; hosted execution remains available |
