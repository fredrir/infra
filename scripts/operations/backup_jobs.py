from __future__ import annotations

import argparse
import json
from pathlib import Path
import re
import sys

import yaml

ROOT = Path(__file__).resolve().parents[2]
SECURITY = {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True, "capabilities": {"drop": ["ALL"]}, "seccompProfile": {"type": "RuntimeDefault"}}
RESOURCES = {"requests": {"cpu": "100m", "memory": "256Mi", "ephemeral-storage": "20Gi"}, "limits": {"cpu": "1", "memory": "1Gi", "ephemeral-storage": "20Gi"}}


def resource(kind: str, name: str, namespace: str, spec: dict, api="v1") -> dict:
    return {"apiVersion": api, "kind": kind, "metadata": {"name": name, "namespace": namespace}, "spec": spec}


def secret_env(name: str, key: str, secret="project-runtime") -> dict:
    return {"name": name, "valueFrom": {"secretKeyRef": {"name": secret, "key": key}}}


def render(project: str, data: dict, catalog: dict, images: dict, restic_image: str) -> list[dict]:
    if project not in catalog["projects"]:
        raise ValueError("Project is not registered")
    namespace = catalog["projects"][project]["namespace"]
    documents = []
    for name, config in data.items():
        if name not in {"postgres", "valkey"} or name not in catalog["projects"][project]["capabilities"]:
            raise ValueError("Unsupported project data dependency")
        rpo = config["recovery"]["rpoHours"]
        if type(rpo) is not int or not 1 <= rpo <= 168:
            raise ValueError("RPO must be between 1 and 168 hours")
        step = max(interval for interval in [1, 2, 3, 4, 6, 8, 12, 24] if interval <= max(1, rpo // 2))
        schedule = "*/30 * * * *" if rpo == 1 else f"0 */{step} * * *"
        backup_name = f"{project}-{name}-backup"
        labels = {"app.kubernetes.io/instance": project, "app.kubernetes.io/component": "backup", "infra.fredrir.com/backup": name}
        source_host = f"{project}-{name}.{namespace}.svc.cluster.local"
        if name == "postgres":
            env = [{"name": "PGHOST", "value": source_host}, {"name": "PGCONNECT_TIMEOUT", "value": "10"}, secret_env("PGUSER", "POSTGRES_USER"), secret_env("PGPASSWORD", "POSTGRES_PASSWORD"), secret_env("PGDATABASE", "POSTGRES_DB")]
            command = 'umask 077\npg_dump --format=custom --no-owner --no-acl --file=/backup/database.dump\npg_restore --list /backup/database.dump > /dev/null\nchecksum="$(sha256sum /backup/database.dump | cut -d " " -f 1)"\nprintf \'{"schemaVersion":1,"kind":"postgres-logical","createdAt":%s,"sha256":"%s"}\\n\' "$(date +%s)" "$checksum" > /backup/manifest.json\n'
        else:
            env = [{"name": "VALKEY_HOST", "value": source_host}, secret_env("VALKEYCLI_AUTH", "VALKEY_PASSWORD")]
            command = 'umask 077\nvalkey-cli -h "$VALKEY_HOST" --rdb /backup/dump.rdb\nvalkey-check-rdb /backup/dump.rdb\nchecksum="$(sha256sum /backup/dump.rdb | cut -d " " -f 1)"\nprintf \'{"schemaVersion":1,"kind":"valkey-rdb","createdAt":%s,"sha256":"%s"}\\n\' "$(date +%s)" "$checksum" > /backup/manifest.json\n'
        init = {"name": "logical-dump", "image": images[name]["image"], "command": ["/bin/sh", "-ec"], "args": [command], "env": env, "securityContext": SECURITY, "resources": RESOURCES, "volumeMounts": [{"name": "dump", "mountPath": "/backup"}, {"name": "temporary", "mountPath": "/tmp"}]}
        credentials = f"{name}-backup"
        restic = {"name": "upload", "image": restic_image, "args": ["backup", "/backup", "--host", project, "--tag", name, "--json"], "env": [secret_env("RESTIC_REPOSITORY", "repository", credentials), secret_env("RESTIC_PASSWORD", "password", credentials), secret_env("AWS_ACCESS_KEY_ID", "AWS_ACCESS_KEY_ID", credentials), secret_env("AWS_SECRET_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY", credentials), {"name": "RESTIC_CACHE_DIR", "value": "/cache"}], "securityContext": SECURITY, "resources": RESOURCES, "volumeMounts": [{"name": "dump", "mountPath": "/backup", "readOnly": True}, {"name": "temporary", "mountPath": "/tmp"}, {"name": "cache", "mountPath": "/cache"}]}
        pod = {"metadata": {"labels": labels}, "spec": {"restartPolicy": "Never", "automountServiceAccountToken": False, "enableServiceLinks": False, "priorityClassName": "batch", "nodeSelector": {"node-restriction.kubernetes.io/stateful": "true"}, "securityContext": {"runAsNonRoot": True, "runAsUser": 999, "runAsGroup": 999, "fsGroup": 999, "seccompProfile": {"type": "RuntimeDefault"}}, "initContainers": [init], "containers": [restic], "volumes": [{"name": "dump", "emptyDir": {"sizeLimit": "16Gi"}}, {"name": "temporary", "emptyDir": {"sizeLimit": "128Mi"}}, {"name": "cache", "emptyDir": {"sizeLimit": "1Gi"}}]}}
        cron = resource("CronJob", backup_name, namespace, {"schedule": schedule, "timeZone": "Etc/UTC", "suspend": True, "concurrencyPolicy": "Forbid", "successfulJobsHistoryLimit": 1, "failedJobsHistoryLimit": 3, "startingDeadlineSeconds": 600, "jobTemplate": {"spec": {"backoffLimit": 1, "activeDeadlineSeconds": min(rpo * 3600, 7200), "template": pod}}}, "batch/v1")
        cron["metadata"]["labels"] = {"infra.fredrir.com/backup": name}
        cron["metadata"]["annotations"] = {"infra.fredrir.com/rpo-seconds": str(rpo * 3600), "infra.fredrir.com/dump-capacity": "16Gi"}
        documents.append(cron)
        documents.append(resource("NetworkPolicy", backup_name, namespace, {"podSelector": {"matchLabels": labels}, "policyTypes": ["Egress"], "egress": [{"to": [{"ipBlock": {"cidr": "0.0.0.0/0", "except": ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "127.0.0.0/8"]}}], "ports": [{"port": 443, "protocol": "TCP"}]}]}, "networking.k8s.io/v1"))
        expr = f'(kube_cronjob_spec_suspend{{namespace="{namespace}",cronjob="{backup_name}"}} == 0) unless on(namespace,cronjob) kube_cronjob_status_last_successful_time{{namespace="{namespace}",cronjob="{backup_name}"}}'
        stale = f'time() - kube_cronjob_status_last_successful_time{{namespace="{namespace}",cronjob="{backup_name}"}} > {rpo * 3600}'
        documents.append(resource("PrometheusRule", backup_name, "observability", {"groups": [{"name": backup_name, "rules": [{"alert": "ProjectBackupNeverSucceeded", "expr": expr, "for": "15m", "labels": {"severity": "critical", "project": project}, "annotations": {"summary": "An enabled data backup has never completed"}}, {"alert": "ProjectBackupOverdue", "expr": stale, "for": "5m", "labels": {"severity": "critical", "project": project}, "annotations": {"summary": "A data backup exceeded its configured recovery point objective"}}]}]}, "monitoring.coreos.com/v1"))
    return documents


def generate(root: Path, project_document: dict) -> list[dict]:
    catalog = json.loads((root / "platform/catalog/projects.json").read_text())
    images = json.loads((root / "platform/catalog/data-images.json").read_text())
    restic = yaml.safe_load((root / "platform/versions.yaml").read_text())["images"]["restic"]
    return render(project_document["project"], project_document.get("data", {}), catalog, images, restic)


def main(argv=None) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("project_config", type=Path)
    args = parser.parse_args(argv)
    try:
        config = yaml.safe_load(args.project_config.read_text())
        print(yaml.safe_dump_all(generate(ROOT, config), sort_keys=False))
        return 0
    except (ValueError, KeyError, OSError) as exc:
        print(f"Backup configuration refused: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
