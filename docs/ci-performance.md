# CI performance

| Measurement | Result | Evidence |
| --- | --- | --- |
| CLI compression, Linux amd64, level 6 → 1 | Median 0.706 → 0.245 seconds; 6,458,416 → 7,097,014 bytes; seven samples per level | [Compression samples](../build/evidence/cli-artifact-compression.json) |
| Documentation and evidence push | Check and reconciliation workflows exclude root Markdown, Markdown under `docs`, and JSON under `build/evidence` | [Check](../.github/workflows/check.yml), [reconciliation](../.github/workflows/reconcile.yml) |
| Image assembly experiment | Concurrent check/runtime evaluation reverted after unchanged latency and SIGKILL at 4 GiB | `ntnu` bounded qualification |
| Historical workflow sample | Includes earlier qualification runs and failures; not an ordinary-traffic deployment percentile | [Baseline](../build/evidence/ci-optimization-baseline.json) |
| Frontend deployment below 15 seconds | Unqualified | [Previous observed timeline](../build/rollout/flux-artifacts/rollout.json) |

| Accounting | Rule |
| --- | --- |
| Aggregate checks | Sum recorded check durations; retain the ten-second ceiling |
| Critical path | Current workflow timestamps and Dagger traces; cached receipt timestamps can belong to earlier executions |
| Compression | CPU work only; upload throughput and queueing are excluded |
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
