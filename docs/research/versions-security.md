# Versions and security

| Field | Value |
| --- | --- |
| As of | 2026-09-11 |
| Evidence | Repository configuration, reported Tailscale version, upstream sources |
| Live verification | Read-only checks on fredrir-04/05/07/08/09; installation unchanged |
| Runtime changes | None |
| Version policy | LTS where offered; maintained stable releases elsewhere; exact versions and digests; security patches within the supported track |

## Implementation targets

| Name | Value |
| --- | --- |
| Verified on | 2026-09-12; local evaluation and configuration checks |
| NixOS | 26.05; existing state compatibility versions retained |
| Linux | 6.18.50 longterm in the evaluated NixOS targets |
| Tailscale | 1.102.4; targeted package pin beyond the OS branch package |
| K3s | 1.36.3+k3s1; matching NixOS and Ansible host contracts |
| Collector | Host Alloy 1.16.0 from NixOS 26.05; Kubernetes Alloy 1.19.2; Promtail replaced in desired host configuration |
| Platform services | Exact image/chart pins in [versions.yaml](../../platform/versions.yaml) |
| Live installation | SSH verified; pending canary verification and controlled activation |

| Observed host | Running versions |
| --- | --- |
| `fredrir-04`, `fredrir-05` | NixOS 25.11; Linux 6.12.93; Tailscale client and daemon 1.90.9 |
| `fredrir-07`, `fredrir-08` | Ubuntu 26.04.1 LTS; Linux 7.0.0-30-generic |
| `fredrir-09` | Ubuntu 26.04 LTS; Linux 7.0.0-30-generic |

Legacy Grafana, Loki, Prometheus, OpenTelemetry Collector and blackbox exporter now use verified immutable image references in desired configuration. The legacy OpenTelemetry `debug` exporter remains separate from the new Tempo pipeline and provides no persistent trace storage.

The support matrix below records the pre-refactor baseline and the reason for each upgrade. It does not describe the updated working-tree configuration.

## Baseline support matrix

| Component | Repository / report | Support and target |
| --- | --- | --- |
| NixOS | `nixos-25.11` in [flake.nix](../../flake.nix) | 25.11 security support ended 2026-06-30; use supported 26.05, supported through 2026-12-31 ([release announcement](https://nixos.org/blog/announcements/2026/nixos-2605/)) |
| Linux | No explicit kernel-family override | Evaluate the selected lock; 6.18 and 6.12 are upstream longterm families with projected EOL in December 2028; 6.18 is a candidate subject to boot/hardware verification ([kernel lifecycle](https://www.kernel.org/releases.html)) |
| Tailscale | Reported 1.90.9; corroborated by the [package at the locked nixpkgs revision](https://raw.githubusercontent.com/NixOS/nixpkgs/b6018f87da91d19d0ab4cf979885689b469cdd41/pkgs/by-name/ta/tailscale/package.nix) | Test 1.102.4, released 2026-09-10, as the stable candidate; verify the actual selected Nix package ([changelog](https://tailscale.com/changelog#2026-09-10), [release tracks](https://tailscale.com/docs/reference/tailscale-client-versions)) |
| Prometheus | `3.13.2` | 3.13 is LTS through 2027-07-31; follow maintained patches in this family ([LTS policy](https://prometheus.io/docs/introduction/release-cycle/)) |
| Grafana | `13.1.3` | 13.1.x patch support ends 2027-03-20; patch this family or test a forward upgrade; maintenance windows are not universal LTS ([support policy](https://grafana.com/docs/grafana/latest/upgrade-guide/when-to-upgrade/)) |
| Loki | `3.7.6` | Pin a maintained stable release and compatible chart/configuration; review schema and retention changes ([upgrade guide](https://grafana.com/docs/loki/latest/setup/upgrade/)) |
| Promtail | Enabled by [observability module](../../modules/observability/default.nix) | EOL since 2026-03-02; replace with Alloy ([EOL notice](https://grafana.com/docs/enterprise-logs/latest/send-data/promtail/)) |
| Alloy | Not configured | Pin a stable release using GA components; compatibility has documented exceptions, including upstream changes ([compatibility](https://grafana.com/docs/alloy/latest/reference/release-information/backward-compatibility/), [cadence](https://grafana.com/docs/alloy/latest/reference/release-information/release-cadence/)) |
| OpenTelemetry Collector | `0.158.0`; trace exporter is `debug` | Consolidate collection into Alloy after pipeline verification; current configuration provides no queryable trace storage ([collector configuration](../../services/observability/otel-collector.yaml)) |
| Tempo | Not configured | Pin a stable release with version-matched configuration; use monolithic mode and object storage initially ([deployment planning](https://grafana.com/docs/tempo/latest/set-up-for-tracing/setup-tempo/plan/)) |
| K3s | Not configured | Resolve a supported production `stable` candidate and pin it; minor channels can include EOL releases ([release channels](https://docs.k3s.io/upgrades/manual)) |
| OpenTofu | Locally observed `1.12.6`; [versions.tf](../../tofu/versions.tf) declares `>= 1.6.0` | Native S3 `use_lockfile` requires 1.10+; align the minimum and pin a tested runtime; preserve provider locks and separate provider upgrades ([OpenTofu 1.10](https://opentofu.org/docs/v1.10/intro/whats-new/)) |

Container versions above are desired state from [observability units](../../services/observability/unit.nix), not proof of running versions or current patch completeness.

| Upgrade boundary | Rule |
| --- | --- |
| NixOS state compatibility | Retain existing `system.stateVersion = "25.11"`; this setting does not select the installed OS release ([NixOS guidance](https://wiki.nixos.org/wiki/FAQ/When_do_I_update_stateVersion)) |
| Tailscale package | An OS branch bump alone does not prove remediation; evaluate `services.tailscale.package.version` and verify upstream fixes/backports |
| Package exception | If the supported NixOS lock lacks the required Tailscale fix, use a narrowly pinned package override with build and connectivity verification |
| K3s ownership | Nix owns the host executable/service; Kubernetes GitOps owns workloads; no competing installer or automatic binary updater |
| K3s upgrade order | Servers individually, then agents; no skipped Kubernetes minor versions; the upgrade controller does not enforce skew safety ([K3s upgrade rules](https://docs.k3s.io/upgrades/manual)) |
| Compatibility | Review Kubernetes API/CRD support, charts, image versions, plugins, and persistent data formats together ([Kubernetes skew policy](https://kubernetes.io/releases/version-skew-policy/)) |
| Rollback | Preserve the prior OS generation and recovery access; data-format migrations need their own compatible backup/restore path |
| Patch visibility | Record desired version, running version, support deadline, pending restart/reboot, and last successful verification |

## Tailscale exposure

[Server configuration](../../modules/profiles/server.nix) declares OpenSSH and the [Tailscale module](../../modules/tailscale/default.nix) declares client routing; neither proves that optional features are disabled in live preferences.

| Feature / bulletin | Exposure condition | Published fix |
| --- | --- | --- |
| SSH: TS-2026-004, 006, 009 | Tailscale SSH with restricted users or privileged Unix sockets | 1.98.9; additional SSH corrections in 1.98.10 |
| Serve/Funnel: TS-2026-008 | Reachable HTTP handler; Funnel permits internet requests | 1.98.9 |
| Serve: TS-2026-005 | Nonroot operator and privileged socket targets | 1.98.9 |
| Services: TS-2026-007 | Advertised service alongside loopback listeners | 1.98.9 |
| SSH: TS-2026-010 | `acceptEnv` forwards credentials into process arguments/logs | 1.102.1; rotate exposed credentials |
| 4via6: TS-2026-011 | Advertised site route and peer ACL access | 1.102.3 |
| Web UI: TS-2026-002 | Explicitly enabled interface and authorized port access | 1.98.0 |

Sources: [security bulletins](https://tailscale.com/security-bulletins), [additional SSH corrections](https://tailscale.com/changelog#2026-07-28).

The reported 1.90.9 predates these fixes; exploitability and compromise remain unverified.

## Telemetry contract

| Area | Contract |
| --- | --- |
| Application instrumentation | Applications emit OTLP spans and propagate trace context; infrastructure deployment alone cannot create application traces ([Tempo data source prerequisites](https://grafana.com/docs/grafana/latest/datasources/tempo/configure-tempo-data-source/)) |
| Collection | Alloy receives OTLP and collects node/journal/container telemetry; avoid duplicate collection by overlapping agents |
| Identity | Normalize `project`, `service`, `environment`, and `host`; preserve existing `job`/`unit` labels until dashboards and alerts migrate |
| Correlation | Preserve trace IDs in logs and exemplars where instrumented; do not use trace/request IDs as Loki stream labels |
| Promtail migration | Validate conversion against rendered configuration; compare filters, labels, timestamps, journal access, and file discovery |
| Read position | Verify file-position and journal-cursor compatibility for selected versions; do not assume copying a positions file transfers journal state |
| Cutover | Rehearse with a separate test destination; use one production writer, persistent supported cursor state, and an explicit replay watermark when state cannot migrate |
| Cutover evidence | Emit uniquely identifiable fixture events before/after cutover and restart; check omissions, duplicates, ingestion delay, and label continuity |
| Trace storage | Monolithic Tempo with persistent working storage and object storage; current Tempo 3.0 microservices mode requires Kafka, monolithic mode does not ([architecture](https://grafana.com/docs/tempo/latest/introduction/architecture/)) |
| Trace retention | **Proposed: 7 days**, adjusted to measured ingestion and query needs; enforce deletion and account for object-store version retention |
| Data minimization | Exclude credentials, cookies, request bodies, and unrestricted query strings; redact sensitive attributes before export ([OpenTelemetry guidance](https://opentelemetry.io/docs/security/handling-sensitive-data/)) |
| Trace access | Private endpoints and authenticating proxy; derive tenant headers from authenticated identity, not caller-controlled project labels ([Tempo authentication](https://grafana.com/docs/tempo/latest/operations/authentication/)) |
| Sampling | Begin with bounded ingestion and a measured sampling policy; tail sampling requires trace-affine routing and memory budgets ([Alloy tail sampling](https://grafana.com/docs/alloy/latest/reference/components/otelcol/otelcol.processor.tail_sampling/)) |
| Capacity | Set memory/CPU requests and limits, ingestion ceilings, queue bounds, storage quotas, and retention before raising collection volume |
| Failure signal | Alert on dropped data, full queues, storage exhaustion, missing collectors, and unavailable monitoring; keep an independent watchdog |

## Recovery checks

| Existing behavior | Required evidence |
| --- | --- |
| Weekly database dumps in [backend backups](../../modules/backups/llunde-backend.nix) and [parser backups](../../modules/backups/pyparser.nix) | Per-service acceptable data loss and restore time; cadence consistent with those limits |
| Dumps use `pg_dump -Fc` | Restore with `pg_restore` under the rootless service owner; `psql` cannot consume the custom archive directly ([PostgreSQL 17](https://www.postgresql.org/docs/17/app-pgrestore.html)) |
| [Backup success metric](../../modules/backups/default.nix) exists only after success | A never-successful expected job alerts, as does a stale job; verify both missing and present-but-old metric cases |
| [Restic staleness alert](../../services/observability/grafana/alerting.yaml) uses `noDataState: OK` | Missing expected job metrics must not remain healthy |
| Monthly structural `restic check` | Rotating `--read-data-subset` checks plus application restores; structural checks do not read all pack data ([Restic integrity checks](https://restic.readthedocs.io/en/stable/045_working_with_repos.html#checking-integrity-and-consistency)) |
| Live Valkey AOF directory is copied after asynchronous rewrite request | Demonstrate a consistent restored dataset under concurrent writes; choose an application-consistent snapshot procedure |
| Database/files restored separately | Verify required cross-resource consistency, application readiness, representative records, and measured recovery duration |
| Restore rehearsal | Disposable database/volume, isolated credentials/network, recorded snapshot ID, and no production overwrite |

## Future validation commands

These commands are reference checks, not evidence of execution.

| Context | Command |
| --- | --- |
| On each selected NixOS node; read-only | `nixos-version` |
| Running kernel; read-only | `uname -r` |
| Tailscale CLI and daemon; read-only | `tailscale version --daemon` ([CLI reference](https://tailscale.com/kb/1080/cli)) |
| Current flake target; desired package | `nix eval --no-write-lock-file --raw '.#nixosConfigurations.llunde-01.config.services.tailscale.package.version'` |
| Current parser target; desired package | `nix eval --no-write-lock-file --raw '.#nixosConfigurations.llunde-parser.config.services.tailscale.package.version'` |
| Desired kernel; repeat for each target | `nix eval --no-write-lock-file --raw '.#nixosConfigurations.llunde-01.config.boot.kernelPackages.kernel.version'` |
| Isolated Linux checkout; pinned formatter | `alejandra --check .` |
| Isolated Linux checkout | `nix flake check --no-write-lock-file` |
| Build without activation; isolated Linux builder | `nix build --no-write-lock-file --no-link '.#nixosConfigurations.llunde-01.config.system.build.toplevel' '.#nixosConfigurations.llunde-parser.config.system.build.toplevel'` |
| Explicit rehearsal cluster context | `kubectl --context=infra-rehearsal get --raw='/readyz?verbose'` |
| Three control-plane nodes / agents; read-only | `kubectl --context=infra-rehearsal get nodes -o wide` |
| Workload state; read-only | `kubectl --context=infra-rehearsal get pods -A` |
| Restored archive inspection; read-only | `pg_restore --list ./restore/service.dump` |

| Verification stage | Pass condition |
| --- | --- |
| Package build | Intended versions in the evaluated closure; no unexpected service/data migrations |
| Host canary | Administrative connectivity, DNS, workloads, and telemetry survive service restart/reboot before advancing |
| K3s rehearsal | Three ready control-plane nodes, etcd quorum, healthy workloads, and continued API availability during a controlled single-server interruption |
| Telemetry rehearsal | Fixture log/trace query succeeds; collector restart does not produce unbounded replay; sensitive fixture attributes are absent |
| Recovery rehearsal | Fresh isolated restore starts the application with expected records/files inside the stated recovery limits |
