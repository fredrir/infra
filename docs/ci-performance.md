# CI performance

## Budgets

| Stage | Limit | Source |
| --- | --- | --- |
| Aggregate fast checks | 10 seconds; failed, missing or timed-out receipts fail the gate | [Check pipeline](../internal/pipeline/fast.go), [budget validation](../internal/ci/groupbudget.go) |
| Image preparation | Measured separately from checks | [Image workflow](../.github/workflows/build-image.yml) |
| Deployment promotion | 30 seconds | [Deployment workflow](../.github/workflows/deploy.yml) |
| Production publication wait | 8 minutes | [Deployment workflow](../.github/workflows/deploy.yml) |
| Frontend revision readiness | 60 seconds after publication; 65-second measurement wrapper | [Deployment workflow](../.github/workflows/deploy.yml) |

The 15-second frontend delivery target remains unqualified. A passing check budget does not establish end-to-end deployment latency.

## Measurement

```sh
infra ci timeline fredrir/llunde-frontend "$BUILD_RUN" --deployment-run "$DEPLOY_RUN" --budget 15s
infra ci wait-revision --url https://llunde.no/.well-known/revision --revision "$SOURCE_REVISION" --budget 60s
infra ci measure --stage checks --budget 10s --report-dir /tmp/infra-performance -- COMMAND
```

| Measure | Rule |
| --- | --- |
| Checks | Sum recorded check durations; report dependency preparation separately |
| Delivery | Workflow creation through the expected source revision served over HTTPS; HTTP 200 alone is insufficient |
| Critical path | Use workflow timestamps and Dagger traces; cached receipts can contain timestamps from earlier executions |
| Clocks | Compare timestamps from the same clock; do not subtract an estimated offset from historical observations |
| Percentiles | Use representative ordinary traffic; separate canaries, unchanged inputs, application changes and dependency changes |
| Resources | Record CPU, memory, queueing and failures alongside latency; identify whether measurements include the engine, hosted jobs or cluster nodes |
| Workflow attempts | [Completion observer](../.github/workflows/performance.yml) retains timings for successful and failed attempts for 30 days |
| Local benchmarks | [Development commands](development.md#local-development), [benchmark scenarios](../dev/bench/scenarios.yaml) |

## Execution controls

| Control | Source |
| --- | --- |
| Verified CLI reuse and input identity | [CLI workflow](../.github/workflows/infra-cli.yml), [input digest](../.github/actions/cli-inputs/action.yml) |
| Affected checks, preparation and generated BUILD validation | [Check workflow](../.github/workflows/check.yml), [pipeline](../internal/pipeline) |
| Bazel cache identities, access and recovery | [Runbook](runbook.md#bazel-cache) |
| Runner admission, resource limits and draining | [Runbook](runbook.md#ci-execution-and-runner-admission), [production inventory](../ansible/inventory/production.yml) |
| Repository engine isolation and cache retention | [Build engine role](../ansible/roles/build_engine) |
| Scanner database sharing, freshness and analysis caches | [Scanner implementation](../internal/ci/scanner.go), [image workflow](../.github/workflows/build-image.yml) |
| Reconciliation scope, checkpoints and repair | [Runbook](runbook.md#reconciliation) |
| Rust target archive selection | [Project profiles](../build/projects), [cache implementation](../internal/ci/cache.go) |
| Kustomize equivalence | `infra dev qualify kustomize` compares repository renders with `kubectl kustomize` |

## Qualification evidence

Records describe their measured revisions and workloads; they are not current fleet benchmarks or ordinary-traffic percentiles. Detailed runs and failed attempts remain in [build/evidence](../build/evidence).

| Qualification | Record |
| --- | --- |
| Execution costs and Rust archive tradeoffs | [Execution measurements](../build/evidence/ci-execution-optimization.json) |
| Frontend startup and clock limitations | [Startup comparison](../build/evidence/frontend-delivery-startup.json) |
| Artifact ownership and frontend delivery | [Flux qualification](../build/evidence/flux-artifact-qualification.json) |
| Four-job admission and repository isolation | [Admission capacity](../build/evidence/build-vm-fourth-slot.json), [engine isolation](../build/evidence/build-vm-repository-isolation.json) |
| Bazel latency and cache trust | [Latency replay](../build/evidence/bazel-remote-latency.json), [CI qualification](../build/evidence/bazel-cache-ci.json) |
| Scanner database sharing | [Scanner qualification](../build/evidence/scanner-cache-sharing.json) |
| Package publication and installation | [Package qualification](../build/evidence/package-optimization-qualification.json), [production canary](../build/evidence/package-production-canary.json) |
