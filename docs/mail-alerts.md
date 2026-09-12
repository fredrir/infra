# Email alerts

| Setting | Value |
| --- | --- |
| Sender | `alerts@fredrir.com` |
| Region / transport | `eu-north-1`; `email-smtp.eu-north-1.amazonaws.com:587`; verified STARTTLS |
| IAM owner | OpenTofu `module.platform_mail`; dedicated `fredrir-platform-alerts-smtp` user |
| SMTP authority | Exact sending-domain and verified-recipient identities, sender and every recipient; matching grant and permissions boundary |
| Recipient | Private settings and Doppler `llunde/ops/PLATFORM_ALERT_RECIPIENT` |
| SMTP username | Doppler `llunde/ops/PLATFORM_WATCHDOG_SMTP_USERNAME` |
| SMTP password | Doppler `llunde/ops/PLATFORM_WATCHDOG_SMTP_PASSWORD` |
| AWS secret access key | Used only in memory for regional SMTP derivation; never stored |
| Local settings | Private `0600` `.infra/email-alerts/smtp-settings.json` inside a `0700` directory |
| Issuance receipt | Private `.infra/email-alerts/smtp-issue.json`; key ID, status and settings hash, no credential values |
| External host | `fredrir-06`, root at Tailnet `100.86.241.75`, existing SSH host-key alias `linode`; existing Y workloads remain in place |
| Runtime config | Root-owned `0600` `/var/lib/platform-watchdog-config/config.json`; persistent across reboot |
| Runtime access | systemd `LoadCredential`; watchdog `DynamicUser` receives only this config |
| Credential modes | `0400`/`0600`, or root-owned `0440` with the exact ACL granting read access only to root and the service UID |
| Health requests | `User-Agent: fredrir-platform-watchdog/1.0`; redirects refused and expected HTTP status enforced |
| Verified delivery |2026-09-12: Linode SMTP accepted; recipient confirmed Gmail receipt and DKIM PASS for `fredrir.com` |
| Current monitoring | `llunde-public`; expected HTTP200; timer enabled and successful; Y services remain active |
| Applied state | OpenTofu serial13; full post-apply plan has zero changes |

The SMTP password is derived from a long-term IAM key using AWS's regional algorithm; SMTP passwords and AWS secret keys differ. [SES credentials](https://docs.aws.amazon.com/ses/latest/dg/smtp-credentials.html)

| Step | Command from repository root |
| --- | --- |
| Offline settings check | `uv run --frozen --group ci python scripts/operations/mail_credentials.py check .infra/email-alerts/smtp-settings.json` |
| Provision credentials | `uv run --frozen --group ci python scripts/operations/mail_credentials.py issue .infra/email-alerts/smtp-settings.json --receipt .infra/email-alerts/smtp-issue.json` |
| Verify stored credentials | `uv run --frozen --group ci python scripts/operations/mail_credentials.py verify .infra/email-alerts/smtp-settings.json --receipt .infra/email-alerts/smtp-issue.json` |
| Optional local delivery test | `uv run --frozen --group ci python scripts/operations/mail_credentials.py test-email .infra/email-alerts/smtp-settings.json --receipt .infra/email-alerts/smtp-issue.json` |
| Deliver private config | `uv run --frozen --group ci python scripts/operations/mail_credentials.py deliver-config .infra/email-alerts/smtp-settings.json --receipt .infra/email-alerts/smtp-issue.json` |
| Preview host changes | `uv run --frozen --group ci ansible-playbook -i .infra/external-watchdog/inventory.yml ansible/site.yml --limit fredrir-06 --check --diff` |
| Install and enable timer | `uv run --frozen --group ci ansible-playbook -i .infra/external-watchdog/inventory.yml ansible/site.yml --limit fredrir-06` |
| Recover interrupted issuance | `uv run --frozen --group ci python scripts/operations/mail_credentials.py recover .infra/email-alerts/smtp-settings.json --receipt .infra/email-alerts/smtp-issue.json` |

| Provisioning check | Required behavior |
| --- | --- |
| AWS / SES | Exact account, verified sender and DKIM, healthy sending account, verified recipient when sandboxed |
| IAM | Exact configured policy and boundary; no existing access keys for initial issuance |
| Doppler | Existing SMTP keys refused; an existing matching recipient preserved; secret values travel through stdin |
| Failure | Deactivate/delete the new IAM key, then remove its new Doppler entries; retain `recovery-required` if reconciliation is ambiguous or unavailable |
| Recovery | Receipt-bound account and key; refuse to revoke a successfully stored credential through the failure-recovery command |
| Rotation | Separate reviewed rotation procedure; initial issuance never replaces an active credential |
| Config delivery | Strict SSH host keys, verified host identity, private atomic install; identical retry allowed, changed existing config refused |

Doppler secret writes use the CLI's stdin interface; values are never passed as command arguments. [Doppler CLI source](https://github.com/DopplerHQ/cli/blob/master/pkg/cmd/secrets.go)

After installation, connect with `ssh -o HostKeyAlias=linode root@100.86.241.75` and run the delivery test with the service's credential and isolation model:

```sh
systemd-run --unit=platform-watchdog-delivery-test --wait --collect --pipe \
  --service-type=exec -p DynamicUser=yes -p RuntimeMaxSec=60s \
  -p MemoryMax=96M -p CPUQuota=20% -p NoNewPrivileges=yes \
  -p PrivateTmp=yes -p PrivateDevices=yes -p ProtectSystem=strict -p ProtectHome=yes \
  -p LoadCredential=config:/var/lib/platform-watchdog-config/config.json \
  /usr/bin/python3 /usr/local/lib/platform-watchdog.py --config %d/config --test-email
systemctl start platform-watchdog.service
systemctl status platform-watchdog.timer platform-watchdog.service --no-pager
```

| Acceptance | Evidence |
| --- | --- |
| Delivery | SMTP accepted from06 and the configured recipient received the test message; inspect DKIM authentication in the received message |
| Incident state | `--test-email` performs no health probes or incident-state writes |
| Timer | Enabled/active; normal checks succeed and Y baseline health remains unchanged |
| Failure retry | Failed SMTP delivery leaves the previous incident state intact |
| Reboot | Persistent root config exists and the timer returns without a Doppler token on the host |

Future full OpenTofu plans retrieve the private recipient without putting it in public production settings:

```sh
uv run --frozen --group ci python scripts/operations/mail_credentials.py plan-input \
  .infra/email-alerts/smtp-settings.json --output .infra/email-alerts/next-recipient.tfvars.json
tofu -chdir=tofu plan -input=false -var-file=production.tfvars.json \
  -var-file=../.infra/email-alerts/next-recipient.tfvars.json \
  -out=../.infra/email-alerts/next.tfplan
```

The recipient file must be fresh and private; enabled mail without it fails closed. Review the complete saved plan before applying it. [OpenTofu mail commands and state recovery](../tofu/modules/platform-mail/README.md)
