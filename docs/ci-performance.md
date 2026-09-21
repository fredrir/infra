# CI performance

| Measurement | Result | Evidence |
| --- | --- | --- |
| CLI compression, Linux amd64, level 6 → 1 | Median 0.706 → 0.245 seconds; 6,458,416 → 7,097,014 bytes; seven samples per level | [Compression samples](../build/evidence/cli-artifact-compression.json) |
| Documentation and evidence push | Check and reconciliation workflows exclude root Markdown, Markdown under `docs`, and JSON under `build/evidence` | [Check](../.github/workflows/check.yml), [reconciliation](../.github/workflows/reconcile.yml) |
| Image assembly experiment | Concurrent check/runtime evaluation reverted after unchanged latency and SIGKILL at 4 GiB | `ntnu` bounded qualification |
| Isolated deployment integration tests, Linux | Median 4.976 → 2.051 seconds with four concurrent fixtures; CPU 5.680 → 5.648 seconds; three samples each | [Fixture samples](../build/evidence/ci-fixture-parallelism.json) |
| Scanner database sharing, Linux | Five paths share one 1.414 GB vulnerability database; warm preparation 10–20 ms; separate analysis cache per image | [Scanner qualification](../build/evidence/scanner-cache-sharing.json) |
| Package publication structure | Three jobs → two; four artifact downloads → two; two hosted engines → one | [Package qualification](../build/evidence/package-optimization-qualification.json) |
| Package smoke payload | Logical fixture inputs 25,024,491 → 5,664,372 bytes; 39 cache-selection assertions; real signed installs and corrupt-package rejection | [Package qualification](../build/evidence/package-optimization-qualification.json) |
| Engine reconciliation | Exact cached image digests skip registry pulls; each changed service restarts independently | [Runner qualification](../build/evidence/scanner-cache-sharing.json) |
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
| Package smoke process startup and workflow handoff | [Package qualification](../build/evidence/package-optimization-qualification.json): unchanged-input CLI medians 1.458 → 1.467 seconds; local signed fixture, no steady-warm latency improvement | Qualify the merged build/smoke job against the production catalog; retain the ten-second installation gate and separate publish credentials |

| Scanner rollout constraint | Requirement |
| --- | --- |
| Shared storage | `trivy-v2` family directories and shared database root must use the same filesystem |
| Freshness | Pinned Trivy metadata policy; failed or stale refresh blocks preparation |
| Concurrency | Shared refresh lock; per-family scan lock; immutable database replacement |
| Legacy cache | Retain until old pinned workflow jobs drain and consumers cut over |
| Rollback | Previous CLI/workflow pins and legacy cache remain usable |
