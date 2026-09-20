# Kata runtime

| Input | Value |
| --- | --- |
| Orchestrator | `infra kata build` with compiled Dagger SDK |
| Builder | Native Linux amd64; dedicated isolated Dagger engine VM |
| Resource ceiling | Two CPUs, 4 GiB RAM, no swap, 256 processes |
| Engine verification | Local Docker inspection of immutable engine container ID |
| Guest privilege | Explicit `--allow-privileged-guest`; isolated engine VM only |
| Source and release pins | `ansible/roles/ci_runtime/files/kata-runtime.json` |
| Output | Runtime archive and per-file checksum manifest |
| Initrd | Go cpio/gzip writer; deterministic device metadata |
| Activation | Native qualification followed by Ansible |

```sh
infra kata build qemu --root . --work-dir "$KATA_WORK" \
  --binary "$INFRA_LINUX_AMD64" --engine-container "$DAGGER_ENGINE"
infra kata build kernel --root . --work-dir "$KATA_WORK" \
  --binary "$INFRA_LINUX_AMD64" --engine-container "$DAGGER_ENGINE"
infra kata build guest --root . --work-dir "$KATA_WORK" \
  --binary "$INFRA_LINUX_AMD64" --engine-container "$DAGGER_ENGINE" \
  --allow-privileged-guest
infra kata build virtiofsd --root . --work-dir "$KATA_WORK" \
  --binary "$INFRA_LINUX_AMD64" --engine-container "$DAGGER_ENGINE"
infra kata build package --root . --work-dir "$KATA_WORK"
```

The selected engine must enforce the resource ceiling; Dagger child cgroup namespaces can hide ancestor limits, so the client verifies the engine and the worker enforces CPU affinity.

| Validation | Evidence |
| --- | --- |
| Unit tests | `go test ./internal/kata` |
| Build inputs | Verified source revisions, archive hashes and signed package snapshot |
| Build output | Atomic archive assembly; exact regular-file allowlist |
| Runtime | Candidate archive matches installed files; KVM, run, exec, virtiofs and cgroup checks |
| Publication | Accepted archive checksum and successful native qualification evidence |

```sh
infra kata qualify --disposable-host \
  --archive "$KATA_ARCHIVE" --manifest "$KATA_MANIFEST" \
  --image "$QUALIFICATION_IMAGE_DIGEST" --output "$QUALIFICATION_REPORT"

ansible-playbook -i ansible/inventory/production.yml ansible/ci-runtimes.yml \
  --limit fredrir-09 -e ci_kata_enabled=true
```

Qualification requires an installed candidate on a disposable native host; it does not install or activate the runtime on production hosts.
