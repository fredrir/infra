# CI performance

| Measurement | Result | Evidence |
| --- | --- | --- |
| CLI compression, Linux amd64, level 6 → 1 | Median 0.706 → 0.245 seconds; 6,458,416 → 7,097,014 bytes; seven samples per level | [Compression samples](../build/evidence/cli-artifact-compression.json) |
| Documentation and evidence push | The shared reconciliation workflow excludes root Markdown, Markdown under `docs`, and JSON under `build/evidence` | [Workflow](../.github/workflows/reconcile.yml) |
| Frontend source COPY filtering, Linux amd64 | Static export median 13.643 → 11.885 seconds; engine CPU 35.460 → 29.090 seconds; two application samples per variant | [Source filter qualification](../build/evidence/frontend-source-filter-linux-amd64.json) |
| Image assembly experiment | Concurrent check/runtime evaluation reverted after no measured application benefit | [Rejected experiments](../build/evidence/frontend-source-filter-linux-amd64.json) |
| Declaration validation, four CPUs under gVisor | `dbb61bf..a6cccdc` 1.667 → 0.636 seconds, CPU 2.75 → 1.54 seconds; all declarations 4.510 → 3.044 seconds; Ansible-only 0.527 → 0.508 seconds; medians of three `measure-check` samples; hosted push run 36184579253 was killed at its 4.62-second remainder | [Validator](../internal/ci/validate.go) |
| Runner-check Ansible bytecode | Syntax check of 17 playbooks 0.814 → 0.556 seconds under gVisor; two samples each; a second concurrent `ansible-playbook` process changed nothing | [Containerfile](../images/runner-check/Containerfile) |
| Isolated deployment integration tests, Linux | Median 4.976 → 2.051 seconds with four concurrent fixtures; CPU 5.680 → 5.648 seconds; three samples each | [Fixture samples](../build/evidence/ci-fixture-parallelism.json) |
| Scanner layer analysis warm-up, Linux amd64 | `vulnerability-scan` runner-rust 8.65 s (killed) → 1.09 s; runner-check 6.22 s (killed) → 1.10 s; one-off `scanner-analysis` preparation 50.36 s and 16.73 s; warm images 1.0–1.3 s | [Analysis warm-up](../build/evidence/runner-scanner-analysis.json) |
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
| Private provenance verification, Linux amd64 | Prepared Sigstore trusted root 2.03 → 1.63 seconds median; TUF cache alone changes nothing; wrong signer, missing trusted root and unsigned digest still fail | [Trust preparation](../build/evidence/private-provenance-trust-preparation.json) |
| Parser production canary | Provenance verification 3.297 → 2.602 seconds; aggregate checks 9.958 → 9.394 seconds of the ten-second ceiling; dependency image reused | [Parser canary](../build/evidence/parser-final-canary.json) |
| Mitogen host reconciliation | Hosts 469.3 → 211.1 seconds; monitor 49.3 → 21.8; verify 62.9 → 48.8; one production sample each; `INFRA_ANSIBLE_STRATEGY=linear` restores the linear strategy | [Mitogen qualification](../build/evidence/ansible-mitogen.json) |
| Project-scoped reconciliation planning | Production plan stage 45.489/51.090 → 5.285 seconds for an application-only change; full recursive render 0.525 → 0.097 seconds locally; project documents match the full render exactly for `llunde` (27/27) and `y` (33/33) | [Scoped planning](../build/evidence/reconcile-project-scoped-plan.json), [production sample](../build/evidence/reconcile-scoped-plan-production.json) |
| Public consumer canaries | Portfolio checks 3.841/6.134/4.355 seconds and Y checks 3.196/4.208 seconds with public provenance verification 0.64–1.15 seconds; all five images reconciled into production | [Portfolio canary](../build/evidence/portfolio-final-canary.json), [Y canary](../build/evidence/Y-final-canary.json) |
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
| Runner dependency layer identity | [Actual rejected reorder](../build/evidence/dagger-copy-layer-identity.json): a small fixture retained its tool layer, but the real image did not; runner-check re-analyses 146 MB after each CLI change inside the `scanner-analysis` preparation stage | Separate stable dependency assembly from changing CLI payloads; require actual image evidence before adoption |
| CLI release caches | [Cache scope](../build/evidence/cli-release-cache-scope.json): sibling release tags cannot restore each other's caches, and no matching cache is produced on the default branch | Qualify a trusted default-branch cache producer or persistent release cache without adding speculative compilation to ordinary commits |
| Signed package build | [Production canary](../build/evidence/package-production-canary.json): build step 97 seconds; installation checks 7.487 seconds | Profile catalog downloads and repeated attestation verification; preserve the ten-second installation gate and separate publish credentials |
| Runner queueing | Whole-job admission limits the build VM to three expensive jobs; short canaries do not establish queue percentiles | Compare admission wait, GitHub queue time and memory pressure before increasing slots or moving workloads |
| Application delivery handoff | [Frontend sample](../build/evidence/frontend-delivery-startup.json): 137.4 seconds from workflow creation to hosted verification; publication wait 37.3 seconds and Kubernetes reconciliation 20.1 seconds | Profile remaining workflow startup, protected environment setup and rollout readiness separately |
| Full host reconciliation | [Task spans](../build/evidence/reconciliation-host-overhead.json): 435 task starts and 637.174 seconds total in one historical run; smart gathering already caches facts within a run | Profile remaining role work and preserve drift detection and registration checks |
| Production revision publication | Full infrastructure convergence still serializes application publication; a concurrent frontend change exceeded its original serving deadline during qualification | Keep publication queueing visible and bounded; isolate application delivery further only with an equivalent infrastructure and artifact baseline |

| Scanner rollout constraint | Requirement |
| --- | --- |
| Shared storage | `trivy-v3` family directories and shared database root must use the same filesystem |
| Freshness | Pinned Trivy metadata policy; failed or stale refresh blocks preparation |
| Concurrency | Shared refresh lock; per-family scan lock; immutable database replacement |
| Database lifetime | Per-attempt leases release family database links after jobs; six-hour maintenance removes expired abandoned leases under family locks |
| Analysis warm-up | `scanner-analysis` preparation stage, 5 m budget, exact `vulnerability-scan` arguments; the 10 s check then reads a warm analysis cache |
| Legacy cache | Database copies removed after draining listeners; analysis caches retained and seeded into the new namespace |
| Rollback | Previous CLI/workflow pins can reuse retained analysis and download current databases; remove the new prune command before downgrading the installed CLI |

## Execution controls

| Control | Behavior |
| --- | --- |
| Shared CLI | Checks and image planning consume one verified CLI artifact; reconciliation directly installs the matching pinned release or awaits the shared build when CLI inputs differ |
| Production validation | Apply validates declarations and live preflight before mutation; checks run alongside reconciliation; production application is serialized |
| Application setup | Durable applied state selects tools; application-only changes skip host tooling, SSH and runner-registration credentials; recovery retains full setup |
| State operations | Signed S3 requests reuse HTTP connections with conditional lease ownership, encryption and durable status updates |
| Permission preflight | Up to four read-only workload permission checks run concurrently before mutation |
| Scheduled work | Hourly deep verification requested by the `fredrir-06` timer reads live state and the recorded reconciliation status and compares OpenTofu and each host playbook concurrently in check mode; merges run the selected `infra reconcile apply`; a full reconciliation runs when a verification report lists differences, including an incomplete or superseded apply |
| Deployment selection | Explicit CI-only paths skip deployment; supported project changes select the union of their dependency chains; tooling inputs never widen scope; shared and unknown inputs retain full fallback |
| Host playbook selection | Role and playbook changes converge only the `reconcile.yml`, `external.yml` and `volatile.yml` playbooks whose role closure changed, after `facts.yml`; inventory, configuration, plugins, shared files, `tofu/`, `secrets/`, shared, unknown, full and recovery inputs converge every playbook |
| Scoped runner convergence | Runner play and `verify-runners.yml` run only when selected or when GitHub reports runners offline, missing or at another version or label set; registrations are always verified |
| K3s service installation | The upstream installer reruns only when its recorded file hashes differ; check mode reports the difference |
| Tooling changes | `cmd/`, `internal/`, `.github/`, Go and Bazel modules and listed `build/` inputs run read-only drift verification of the applied revision: generated artifacts, `tofu plan -detailed-exitcode`, workloads and hosts; failure forces full recovery |
| Workload verification | Each verification poll lists workloads once per kind and namespace; expected ownership, images, readiness and generations remain required |
| Transport installation | Pinned archive content is compared with extracted and installed binaries before extraction or copy; missing or corrupted files are repaired |
| Declaration validation | Changed-scope checks run concurrently up to `GOMAXPROCS`; each check's output is released in check order once it and every earlier check finish; errors follow check order; a panic fails only its check; kustomize renders in-process, byte-identical to `kubectl kustomize` per `infra dev qualify kustomize`; a contract test ties the linked kustomize modules to the pinned `kubectl` and `kustomize` |
| Rust target cache | Cache save identity includes source contents, lockfile and build arguments; dependency outputs remain reusable across source changes |
| VM admission | A configurable host-wide limit bounds complete jobs across repository listeners; leases use worker PID and process start time and are reclaimed after worker exit |
| Timing | A read-only completion observer retains job and step timestamps for successful and failed workflow attempts for 30 days |
| Frontend deadlines | Publication queueing has a separate eight-minute measurement; the subsequent exact served-revision check retains its 60-second budget within the existing ten-minute job |

VM admission requires an installed CLI with `platform runner-admission` before `build_runner_admission_enabled` is enabled; production admits three jobs on the 16 GiB build VM.
An hourly verification report that lists differences dispatches one full reconciliation of `main` unless a push reconciliation on `main` has not completed its apply or the latest bot dispatch for the same commit ended in `failure`, `timed_out` or `startup_failure`, or started within six hours and was not cancelled; a new commit on `main` lifts the cap; `verify=true` without `repair=true` never dispatches.
These controls do not establish an ordinary-traffic latency percentile; compare the completion observer's post-rollout samples with equivalent workloads.

## Execution measurements

[Execution evidence](../build/evidence/ci-execution-optimization.json) records production qualification on 2026-09-24, exact workflow attempts, job queues, setup and preparation steps, checks, publication, reconciliation and HTTPS revision observation.
Failed attempts remain part of the evidence, including the frontend serving deadline failure and its successful recovery rerun.

| Measurement | Result | Scope |
| --- | --- | --- |
| Scheduled full reconciliation | 48 → 0 runs/day, plus 24 read-only deep verifications/day | Configured frequency; repairs run only for listed differences; daily resource savings are not yet measured |
| Workload API reads | 49 → 16 kubectl calls; median 6.21 → 2.08 seconds; client CPU 2.10 → 0.67 seconds | Three alternating samples per mode, 49 matching resources over the local tailnet; excludes rollout waiting |
| Read-only live verification | 36.76 seconds; 5.26 client CPU seconds; no Ansible changes | Local tailnet execution of the live checks without the OpenTofu and host check-mode comparisons of the hourly `--scope=full` verification |
| Whole-job admission | One active lease; a second worker timed out without displacing the owner | Installed production CLI; live acquire and release hooks verified |
| Active build memory | Runner slice 1.81 GiB peak; engine 3.62 GiB peak; no OOM event increase | Twenty-second sample at four samples/second; includes page cache |
| External frontend observation, raw | 195.33 seconds from workflow creation to local observation | Clocks were not calibrated; use the hosted comparison below for percentages; the initial 60-second serving wait failed |
| Frontend scoped apply | Plan 10.89 seconds; publish 1.96; Kubernetes 18.63; verify 7.42 | Only `llunde` selected; no host or OpenTofu application |
| Frontend VM resource use | Runner CPU 26.00 seconds and engine CPU 9.38 seconds; peaks 1.83/3.36 GiB; no OOM event increase | Cgroup samples across delivery; includes background activity and excludes hosted CI and cluster node CPU |

| Rust target archive condition | Job | Restore | Preparation | Save |
| --- | ---: | ---: | ---: | ---: |
| Refresh outputs | 208 s | 45 s | 19 s | 115 s |
| Reuse identical outputs | 83 s | 36 s | 18 s | 1 s |
| Archive disabled | 132 s | — | 102.65 s | — |

The Rust samples use unchanged `nsql` Rust source with workflow metadata changes; they are single observations, not a cold compiler-cache comparison or a latency percentile.
Source-content fingerprint tests verify that source-only changes refresh the target archive, while identical inputs skip publication.
The restored archive still recompiled the workspace in 7.60–7.84 seconds.
`nsql` sets `build-output-cache: false` to avoid archive refresh costs on source-changing builds while retaining compiler `sccache`; repeated identical builds can benefit from enabling the archive.
The reusable workflow defaults the archive to enabled and records environment, restore, preparation, save and checks independently.
The no-archive checks took 4.29 seconds under the unchanged ten-second ceiling.

The build VM admits three jobs with a 4 GiB runner slice and a 10 GiB engine at three parallel operations; guest OOM kills prefer the runner slice (`--oom-score-adj=-500` on the engine), and the `build-vm` cgroup alerts report OOM kills and working sets near either limit.
Workflow completion reports retain attempt identity and queue timestamps for 30 days; compare total delivery latency, failures and resource use alongside the check metric.

## Frontend delivery startup

[Delivery evidence](../build/evidence/frontend-delivery-startup.json) records CLI v0.2.8 activation and the first-attempt production canary for source `78f2bfd936ebf80ec401ee2877553521b4e5b153`.
The build, deployment and reconciliation workflows succeeded, and the expected source revision was served over HTTPS.

| Measurement | Before | After |
| --- | ---: | ---: |
| Build workflow creation → hosted reconciliation verification complete | 187.67 s | 137.37 s, 26.8% lower |
| Reconciliation workflow creation → verification complete | 96.67 s | 59.37 s, 38.6% lower |
| Reconciliation plan | 10.89 s | 6.01 s |
| Reconciliation setup step | 16 s | 10 s |
| State-read client CPU, local median | 0.304 s | 0.020 s |

The hosted comparison uses the same completion signal in both reconciliation reports, including exact artifact, ownership, generation, workload-readiness and served-revision verification.
The original 195.33-second figure mixed a local observer timestamp with GitHub workflow creation; a later clock comparison found the local clock approximately 14 seconds ahead of GitHub.
That raw observation is retained, and no retrospective clock offset is subtracted from it.

The deployment's own successful serving check completed 133.65 seconds after build workflow creation and 57.85 seconds after promotion finished.
Publication wait took 37.30 seconds and the subsequent serving check took 20.37 seconds, within its unchanged 60-second budget.
Aggregate checks took 7.10 seconds under the unchanged ten-second ceiling.

Across the 173.90-second VM sampling window around the canary, runner CPU was 18.36 seconds and engine CPU was 7.34 seconds, with memory peaks of 1.67 GiB and 3.22 GiB and no OOM event increase.
The sample includes idle time around delivery, background activity and page cache; it excludes hosted CI and Kubernetes node CPU, and admission remained at one job.

These are qualification samples rather than an ordinary-traffic percentile, and application source changed independently between the two workflow-metadata canaries.
Full infrastructure changes can still delay publication beyond the queue budget; failed rollout and deployment attempts are retained in the evidence.
The 15-second delivery target remains unqualified.
