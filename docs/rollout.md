# Infrastructure rollout

| Stage            | Requirement                                                                | Repository action                                                                   |
| ---------------- | -------------------------------------------------------------------------- | ----------------------------------------------------------------------------------- |
| Source           | Go, Bazel, native policy and workflow checks pass                          | Merge the reviewed source revision                                                  |
| CLI release      | Protected `infra-v*` tags; tag commit belongs to `main`                    | Apply [tag rules](../.github/cli-tags-ruleset.json), then publish an `infra-v*` tag |
| Binary trust     | Release attestation, source revision and manifest SHA-256 agree            | Install the exact platform artifact; retain the previous binary                     |
| Images           | Published digest, scan and provenance; compiled CLI included               | Promote tools and runner image pins                                                 |
| External callers | Exact reviewed workflow SHA; matching OctoSTS policy; native test commands | Apply each repository patch from `build/rollout` after its expected blobs match     |
| VM engines       | Dedicated trusted VM, native qualification and registered runner           | Apply `ansible/build-engines.yml`; enable the qualified pool explicitly             |
| Retirement       | All callers moved; old jobs drained; rollback data retained                | Remove legacy runner and Attic service resources                                    |

## Applied cutover

| Name              | Value                                                                                                                          |
| ----------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| Source revision   | `b2c97f0c98c09099d19696089e55f1e95bd6ac1d`                                                                                     |
| External patches  | Applied to ten repositories: nine caller workflows and five trust policies; [receipt](../build/evidence/consumer-cutover.json) |
| Concurrency guard | Expected Git blob for every external file; refresh mismatches before applying                                                  |
| Test migration    | Native Dagger stage checks replace the removed shell helper                                                                    |
| Rollback overlap  | Exact previous and new workflow SHAs accepted; remove previous SHAs after old jobs drain and rollback closes                   |

```sh
git -C "$CONSUMER_CHECKOUT" apply --check "$INFRA_CHECKOUT/build/rollout/$REPOSITORY.patch"
```

## Production receipts

| Name              | Value                                                                                                                           |
| ----------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| Released CLI      | [`infra-v0.1.1`](https://github.com/fredrir/infra/releases/tag/infra-v0.1.1); source `a284f912fa9c5febe375a5b6743db4598b29477c` |
| Backup host CLI   | [`infra-v0.1.0`](https://github.com/fredrir/infra/releases/tag/infra-v0.1.0); source `73a5ca2e6b88b40a6f8c55deae12af4a3e058c78` |
| Linux host binary | SHA-256 `c37acf10dd13009f61374b9fce811877b2a40c33b34a600a67e0c9e9dc39d0c1`                                                      |
| Control backup    | `fredrir-07`; Go command completed successfully on 2026-09-20 at 17:05:08 UTC                                                   |
| Go tools image    | `ghcr.io/fredrir/platform-backup-tools@sha256:5d350bdc39e4bf66e4db23944d63cdac7c1098eda71e9a5fde0364191b33d5e2`                 |
| Monitoring        | Gatus healthy; retired cache endpoint removed; backup heartbeats retained                                                       |
| Build execution   | GitHub-hosted Dagger; dedicated VM pool awaits native qualification                                                             |

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

| Resource                                  | Cutover condition                                                                | Retention                                                                   |
| ----------------------------------------- | -------------------------------------------------------------------------------- | --------------------------------------------------------------------------- |
| BuildKit ARC pools and cache credentials  | Every image and package caller uses the hosted Dagger workflow; old jobs drained | Previous workflow/image pins for rollback                                   |
| Nix ARC pool and Attic reader credentials | Infrastructure checks use Bazel; no remaining Nix callers                        | Previous workflow/image pins for rollback                                   |
| Attic StatefulSet, Service and tunnel     | StatefulSet scaled to zero; Service and tunnel retired                           | Attic namespace, PVC, local data and backups until rollback window closes   |
| Attic monitoring                          | Service retirement approved                                                      | Re-encrypt Gatus configuration with SOPS; keep its integrity metadata valid |
| Attic provider resources                  | Reviewed OpenTofu plan                                                           | Preserve backup objects; resolve `prevent_destroy` explicitly               |
| Legacy platform scripts                   | Published Go tools image promoted with commands                                  | Remove their ConfigMap generators and source files in the same change       |

The current Flux cache Kustomization uses `prune: true`. Retain the `nix-cache` namespace, PVC and backup objects; remove workload, network and tunnel resources separately.

## Evidence

| Check                                   | Evidence                                                                                                                                        |
| --------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| Hosted Bazel and declarations           | [Full check](https://github.com/fredrir/infra/actions/runs/35526426207): 16 tests, generated BUILD files and infrastructure declarations passed |
| Production health and native operations | [Production receipt](../build/evidence/production-rollout.json)                                                                                 |
| CLI cross-compilation                   | [Four-platform build receipts](../build/evidence/cli-crossbuild.json)                                                                           |
| Changed-source Dagger reuse             | [Persistent action-cache experiment](../build/evidence/dagger-disk-cache-linux-amd64.json)                                                      |
| Independent Bazel cache reuse           | [Cold and warm action-cache experiment](../build/evidence/bazel-cache-linux-amd64.json)                                                         |
| Native Linux quality                    | [Race tests and vet at the recorded revision](../build/evidence/go-validation-linux-amd64.json)                                                 |
| Signed package installation             | [Debian, Fedora and Alpine qualification](../build/evidence/packages-linux-amd64.json)                                                          |
| External source state                   | [Applied consumer cutover](../build/evidence/consumer-cutover.json); original [consumer inventory](../build/consumers.json)                     |

| Deferred validation                                 | Gate                                                       |
| --------------------------------------------------- | ---------------------------------------------------------- |
| Full Kata/QEMU/kernel rebuild                       | Dedicated engine and explicit time budget                  |
| Native ARM execution                                | Qualified ARM host                                         |
| Full production restore and Kata runtime activation | Restore evidence and qualified runtime host                |
| VM pool activation                                  | Qualified dedicated VM; hosted execution remains available |
