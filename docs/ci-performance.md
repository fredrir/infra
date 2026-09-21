# CI performance

| Measurement | Result | Evidence |
| --- | --- | --- |
| CLI compression, Linux amd64, level 6 → 1 | Median 0.706 → 0.245 seconds; 6,458,416 → 7,097,014 bytes; seven samples per level | [Compression samples](../build/evidence/cli-artifact-compression.json) |
| Documentation and evidence push | Check and reconciliation workflows exclude root Markdown, Markdown under `docs`, and JSON under `build/evidence` | [Check](../.github/workflows/check.yml), [reconciliation](../.github/workflows/reconcile.yml) |
| Frontend source COPY filtering, Linux amd64 | Static export median 13.643 → 11.885 seconds; engine CPU 35.460 → 29.090 seconds; two application samples per variant | [Source filter qualification](../build/evidence/frontend-source-filter-linux-amd64.json) |
| Image assembly experiment | Concurrent check/runtime evaluation reverted after no measured application benefit | [Rejected experiments](../build/evidence/frontend-source-filter-linux-amd64.json) |
| Isolated deployment integration tests, Linux | Median 4.976 → 2.051 seconds with four concurrent fixtures; CPU 5.680 → 5.648 seconds; three samples each | [Fixture samples](../build/evidence/ci-fixture-parallelism.json) |
| Scanner database sharing, Linux | Five paths share one 1.414 GB vulnerability database; warm preparation 10–20 ms; separate analysis cache per image | [Scanner qualification](../build/evidence/scanner-cache-sharing.json) |
| Eviction and restart observation | Unchanged frontend export 38.407 seconds after eviction → 1.703 seconds after warming and restart; one sample per condition; policy and restart effects not independently isolated | [Eviction and restart evidence](../build/evidence/dagger-production-eviction.json) |
| Cache budget | Dagger maximum 20 → 32 GiB; Bazel unchanged; 22.68 GB physically reclaimed from duplicated scanner databases funds the increase | [Budget evidence](../build/evidence/dagger-cache-budget.json) |
| Package publication structure | Three jobs → two; four artifact downloads → two; two hosted engines → one | [Package qualification](../build/evidence/package-optimization-qualification.json) |
| Production package canary | Signed installation checks 7.487 seconds; build-to-smoke handoff 31 → 0 seconds; publication and HTTPS contents verified | [Production package canary](../build/evidence/package-production-canary.json) |
| Package smoke payload | Logical fixture inputs 25,024,491 → 5,664,372 bytes; 39 cache-selection assertions; real signed installs and corrupt-package rejection | [Package qualification](../build/evidence/package-optimization-qualification.json) |
| Engine reconciliation | Exact cached image digests skip registry pulls; each changed service restarts independently | [Runner qualification](../build/evidence/scanner-cache-sharing.json) |
| Failed publication recovery | Reuse completed host configuration only after a recent durable checkpoint, unchanged host inputs and a fresh no-change expansion proof; local Linux and real OpenTofu fixture checks pass | [Checkpoint qualification](../build/evidence/reconcile-host-checkpoint.json) |
| Frontend production canaries | Expected revisions eventually served; the 60-second serving deadline failed during full host reconciliation and during application-only workflow startup plus planning | [Initial canary](../build/evidence/frontend-production-canary.json), [final canary](../build/evidence/frontend-final-canary.json) |
| Backend production canary | Build/publish 199 seconds cold → 22 seconds warm; scanner preparation 32.65 → 0.017 seconds; checks 6.778 seconds; initial cold attempt failed the unchanged ten-second gate | [Backend canary](../build/evidence/backend-final-canary.json) |
| Historical workflow sample | Includes earlier qualification runs and failures; not an ordinary-traffic deployment percentile | [Baseline](../build/evidence/ci-optimization-baseline.json) |
| Frontend deployment below 15 seconds | Unqualified | [Previous observed timeline](../build/rollout/flux-artifacts/rollout.json) |

| Accounting | Rule |
| --- | --- |
| Aggregate checks | Sum recorded check durations; retain the ten-second ceiling |
| Critical path | Current workflow timestamps and Dagger traces; cached receipt timestamps can belong to earlier executions |
| Compression | CPU work only; upload throughput and queueing are excluded |
| Frontend observation | Each deployment records `served-revision` in its performance artifact; `infra ci timeline` joins build creation to successful revision observation |
| Deployment completion | Expected source revision served over HTTPS; HTTP 200 alone does not qualify |
| Percentiles | Separate ordinary traffic, canaries, unchanged inputs, application changes and dependency changes |
| Ordinary-traffic p95 | Require a representative post-change sample; do not infer from a warm canary |

```sh
infra ci timeline fredrir/llunde-frontend "$BUILD_RUN" --deployment-run "$DEPLOY_RUN" --budget 15s
infra ci wait-revision --url https://llunde.no/.well-known/revision --revision "$SOURCE_REVISION" --budget 60s
```

| Upstream behavior | Reference |
| --- | --- |
| Artifact compression levels | [Upload artifact](https://github.com/actions/upload-artifact#altering-compressions-level-speed-v-size) |
| Push-only path filters | [GitHub workflow syntax](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#onpushpull_requestpull_request_targetpathspaths-ignore) |

| Remaining bottleneck | Evidence | Candidate |
| --- | --- | --- |
| Historical package channel downloads and attestations | [`GitHub.Download`](../internal/packages/catalog.go) requests all six Homebrew, Nix and AUR channel files for each of three retained releases; `Collect` publishes channel files only for the newest release | Download channel files only for the newest release; omit twelve unused channel downloads and individual attestation verifications per project with three releases; preserve verification of every consumed package and metadata asset |
| Runner dependency layer identity | [Actual rejected reorder](../build/evidence/dagger-copy-layer-identity.json): a small fixture retained its tool layer, but the real image did not; changed-CLI archive scanning still exceeded its remaining budget | Separate stable dependency assembly from changing CLI payloads; require actual image evidence before adoption |
| CLI release caches | [Cache scope](../build/evidence/cli-release-cache-scope.json): sibling release tags cannot restore each other's caches, and no matching cache is produced on the default branch | Qualify a trusted default-branch cache producer or persistent release cache without adding speculative compilation to ordinary commits |
| Signed package build | [Production canary](../build/evidence/package-production-canary.json): build step 97 seconds; installation checks 7.487 seconds | Profile catalog downloads and repeated attestation verification; preserve the ten-second installation gate and separate publish credentials |
| Application-only planning | [Frontend plan](../build/evidence/reconcile-targeted-plan-opportunity.json): 45.489 seconds rendering the full cluster for a frontend image pin and receipt | Qualify a project-only render with equivalent live substitutions and full-render fallback for shared or unknown changes |
| Full host reconciliation | [Task spans](../build/evidence/reconciliation-host-overhead.json): 435 task starts; 17 fact-gathering passes; 637.174 seconds total in one run | Reuse valid host facts within a reconciliation and narrow role execution to affected hosts; preserve drift detection and registration checks |
| Production revision publication | Frontend canary promotion reached `main`, then waited for a full infrastructure reconciliation before Flux could consume `production`; the 60-second serving check failed | Qualify publication after infrastructure convergence; reuse completed host work only with a valid checkpoint, unchanged host inputs and a live no-change expansion plan |

| Scanner rollout constraint | Requirement |
| --- | --- |
| Shared storage | `trivy-v3` family directories and shared database root must use the same filesystem |
| Freshness | Pinned Trivy metadata policy; failed or stale refresh blocks preparation |
| Concurrency | Shared refresh lock; per-family scan lock; immutable database replacement |
| Database lifetime | Per-attempt leases release family database links after jobs; six-hour maintenance removes expired abandoned leases under family locks |
| Legacy cache | Database copies removed after draining listeners; analysis caches retained and seeded into the new namespace |
| Rollback | Previous CLI/workflow pins can reuse retained analysis and download current databases; remove the new prune command before downgrading the installed CLI |
