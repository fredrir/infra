# Email alerts

| Setting              | Value                                                                                             |
| -------------------- | ------------------------------------------------------------------------------------------------- |
| Sender               | `alerts@fredrir.com`                                                                              |
| Recipient            | Doppler `infra/ops/PLATFORM_ALERT_RECIPIENT`                                                      |
| SMTP                 | `email-smtp.eu-north-1.amazonaws.com:587`, verified STARTTLS                                      |
| IAM                  | OpenTofu `module.platform_mail`, restricted `fredrir-platform-alerts-smtp` user                   |
| Credentials          | Doppler `infra/ops/PLATFORM_WATCHDOG_SMTP_USERNAME` and `PLATFORM_WATCHDOG_SMTP_PASSWORD`         |
| Cluster alerts       | Alertmanager; `platform/components/observability/alertmanager.secret.sops.yaml`                   |
| Independent monitor  | Gatus on Ubuntu `fredrir-06`, outside Kubernetes                                                  |
| Monitor settings     | `ansible/roles/gatus/templates/config.yaml.j2`; `${NAME}` secrets from `ansible/roles/gatus/files/secrets.sops.yaml` |
| Native configuration | Root `0600` `/etc/gatus/config.yaml` through `LoadCredential`; secrets decrypted at start with the [host key](Secrets.md#host-scoped-secrets) |
| Runtime              | `DynamicUser`, SQLite `/var/lib/gatus/gatus.db`, `MemoryMax=256M`                                 |
| Listener             | Tailnet-only `100.86.241.75:8080`                                                                 |
| Cross-check          | Cluster Prometheus scrapes `/metrics` each minute; `IndependentMonitorDown` after 10 minutes down |
| HTTP checks          | Exact status codes; portfolio follows redirects; authentication boundaries checked separately     |
| Response handling    | Status-only checks do not read response bodies; Y API checks its small GraphQL readiness response |

| Backup heartbeat    | Maximum age |
| ------------------- | ----------- |
| `backups_parser`    | 8 hours     |
| `backups_y`         | 8 hours     |
| `backups_control`   | 8 hours     |
| `backups_portfolio` | 2 hours     |
| `backups_attic`     | 2 hours     |

| Verification heartbeat | Value |
| --- | --- |
| `reconciliation_deep` | GitHub deep verification from `fredrir-06`; maximum age 3 hours; failed run reported immediately with its URL; [runbook](runbook.md#reconciliation) |
| `reconciliation_verification` | Reconciler cloud verification; maximum age 2 hours; alerts after two failed runs in a row with the failing stage; [runbook](runbook.md#reconciler-host) |

Successful backup producers POST to `/api/v1/endpoints/backups_<name>/external?success=true` with separate Bearer tokens from their encrypted repository credentials. Producers keep tokens in private curl configuration files. Tailnet policy permits the control and worker producers to reach the monitor; Kubernetes network policies restrict backup pods to this destination.

```sh
export SOPS_AGE_KEY_FILE="$HOME/.config/age/keys.txt"
export ANSIBLE_CONFIG=ansible/ansible.cfg
uv run --frozen --group ci ansible-playbook ansible/external.yml --limit fredrir-06
sops ansible/roles/gatus/files/secrets.sops.yaml
ssh -o HostKeyAlias=fredrir-06 root@100.86.241.75 systemctl status gatus --no-pager
curl --fail http://100.86.241.75:8080/health
```

SMTP passwords use the [SES regional derivation](https://docs.aws.amazon.com/ses/latest/dg/smtp-credentials.html); they are different from IAM secret access keys. Rotate the restricted IAM credential, update Doppler and both encrypted consumers, reconcile Alertmanager and Gatus, verify SMTP authentication, then retire the old access key. Keep credentials out of command arguments and logs.

[Gatus configuration](https://github.com/TwiN/gatus/blob/v5.36.0/README.md) covers native email alerts, SQLite retention and authenticated external heartbeats.
