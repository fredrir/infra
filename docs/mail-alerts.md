# Email alerts

| Setting | Value |
| --- | --- |
| Sender | `alerts@fredrir.com` |
| Recipient | Doppler `infra/ops/PLATFORM_ALERT_RECIPIENT` |
| SMTP | `email-smtp.eu-north-1.amazonaws.com:587`, verified STARTTLS |
| IAM | OpenTofu `module.platform_mail`, restricted `fredrir-platform-alerts-smtp` user |
| Credentials | Doppler `infra/ops/PLATFORM_WATCHDOG_SMTP_USERNAME` and `PLATFORM_WATCHDOG_SMTP_PASSWORD` |
| Cluster alerts | Alertmanager; `platform/components/observability/alertmanager.secret.sops.yaml` |
| Independent monitor | Gatus on Ubuntu `fredrir-06`, outside Kubernetes |
| Monitor settings | `ansible/roles/gatus/files/config.sops.yaml` |
| Native configuration | Root `0600` `/etc/gatus/config.yaml`, delivered through systemd `LoadCredential` |
| Runtime | `DynamicUser`, SQLite `/var/lib/gatus/gatus.db`, `MemoryMax=256M` |
| Listener | Tailnet-only `100.86.241.75:8080` |
| HTTP checks | Exact status codes; portfolio follows redirects; authentication boundaries checked separately |
| Response handling | Status-only checks do not read response bodies; Y API checks its small GraphQL readiness response |

| Backup heartbeat | Maximum age |
| --- | --- |
| `backups_parser` | 8 hours |
| `backups_y` | 8 hours |
| `backups_control` | 8 hours |
| `backups_portfolio` | 2 hours |
| `backups_attic` | 2 hours |

Successful backup producers POST to `/api/v1/endpoints/backups_<name>/external?success=true` with separate Bearer tokens from their encrypted repository credentials. Producers keep tokens in private curl configuration files. Tailnet policy permits the control and worker producers to reach the monitor; Kubernetes network policies restrict backup pods to this destination.

```sh
export SOPS_AGE_KEY_FILE=/path/to/age-key.txt
export ANSIBLE_CONFIG=ansible/ansible.cfg
uv run --frozen --group ci ansible-playbook ansible/external.yml --limit fredrir-06
sops ansible/roles/gatus/files/config.sops.yaml
ssh -o HostKeyAlias=fredrir-06 root@100.86.241.75 systemctl status gatus --no-pager
curl --fail http://100.86.241.75:8080/health
```

SMTP passwords use the [SES regional derivation](https://docs.aws.amazon.com/ses/latest/dg/smtp-credentials.html); they are different from IAM secret access keys. Rotate the restricted IAM credential, update Doppler and both encrypted consumers, reconcile Alertmanager and Gatus, verify SMTP authentication, then retire the old access key. Keep credentials out of command arguments and logs.

[Gatus configuration](https://github.com/TwiN/gatus/blob/v5.36.0/README.md) covers native email alerts, SQLite retention and authenticated external heartbeats.
