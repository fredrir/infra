# Platform mail

| Setting        | Value                                                                                                                |
| -------------- | -------------------------------------------------------------------------------------------------------------------- |
| Enable         | Root `platform_mail` object; default `null`                                                                          |
| Public inputs  | `domain`, verified Cloudflare `zone_id`, exact `sender`, stable `smtp_user_name`                                     |
| Private input  | Sensitive `platform_mail_recipient`; required whenever enabled                                                       |
| Resources      | One SES domain identity, three DKIM CNAMEs, one IAM user, one sending policy and attachment                          |
| Signing        | AWS-managed Easy DKIM, RSA 2048; no private signing key in state                                                     |
| SMTP authority | `ses:SendRawEmail`, sending-domain and exact verified-recipient identity ARNs, exact From, every To/CC/BCC recipient |
| Transport      | Verified STARTTLS or implicit TLS in Alertmanager and Gatus; SES requires SMTP TLS                                   |
| IAM boundary   | The same sending policy limits and grants the user's permissions                                                     |
| Credentials    | Create and rotate SMTP access keys outside OpenTofu; deliver through private runtime credentials                     |
| Existing DNS   | No apex, MX, SPF, DMARC, MAIL FROM or other zone records managed here                                                |
| DNS ownership  | OpenTofu; these DKIM selectors must not also appear in `platform_dns_records`                                        |
| Verification   | `verified_for_sending` reports AWS state; resource creation does not prove sending readiness                         |
| Deletion       | Identity, selectors, policy and user have `prevent_destroy`; user access keys are not force-deleted                  |

SES SMTP needs `SendRawEmail`; IAM supports exact From and recipient restrictions, including CC/BCC. [SES IAM](https://docs.aws.amazon.com/ses/latest/dg/control-user-access.html)

The sandbox SMTP test evaluated the verified recipient identity as well as the sending domain. Both exact ARNs retain the same From and recipient conditions; adding the recipient resource does not permit sending from that recipient address. Validate both resources in IAM simulation and confirm delivery with an actual SMTP test.

Alertmanager and Gatus require verified TLS before SMTP authentication; SES also requires encrypted SMTP connections. [SES protocols](https://docs.aws.amazon.com/ses/latest/dg/security-protocols.html)

Default SES MAIL FROM has SPF; a custom subdomain requires its own MX and SPF records. Verify DKIM alignment before delivery and review existing mail policy before adding DMARC or a custom MAIL FROM. [SES MAIL FROM](https://docs.aws.amazon.com/ses/latest/dg/mail-from.html)

| Validation                | Command from repository root                                                                                                                                                                                            |
| ------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Format                    | `tofu -chdir=tofu fmt -check -recursive`                                                                                                                                                                                |
| Isolated initialization   | Copy `tofu/` to a private work directory, then `tofu -chdir=<workdir> init -backend=false -lockfile=readonly -input=false`                                                                                              |
| Schema                    | `tofu -chdir=<workdir> validate`                                                                                                                                                                                        |
| Mock plan tests           | `tofu -chdir=<workdir> test -filter=tests/platform-mail.tftest.hcl`                                                                                                                                                     |
| Mock apply tests          | `python3 scripts/operations/test_mail_module.py --provider-directory <workdir>/.terraform/providers`                                                                                                                    |
| Production initialization | `tofu -chdir=<workdir> init -lockfile=readonly -input=false` with the existing S3 backend                                                                                                                               |
| Full saved plan           | `tofu -chdir=<workdir> plan -input=false -var-file=<repo>/tofu/production.tfvars.json -var-file=<private>/platform-mail.tfvars.json -var-file=<private>/platform-mail-recipient.tfvars.json -out=<private>/mail.tfplan` |
| Apply gate                | Review the complete saved plan, DNS inventory, account and sender policy; obtain approval for that artifact                                                                                                             |

| Runtime          | Value                                                                                                                                        |
| ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| Local validation | OpenTofu 1.12.6; supported floor 1.10                                                                                                        |
| Locked providers | AWS 5.100.0, Cloudflare 5.23.0, hcloud 1.68.0                                                                                                |
| Execution        | Local review and mocked CI tests; production state assumed                                                                                   |
| Backend          | S3 `llunde-pyparser-bucket`, key `tofu-state/infra.tfstate`, `eu-north-1`                                                                    |
| Risks            | DNS ownership collision, overbroad sending authority, secret exposure, shared-state blast radius                                             |
| Evidence         | Private plan/state backup, DNS inventory, sender policy, DKIM status and successful inbox receipt                                            |
| Rollback         | Revoke an unused or compromised SMTP key first; retain DNS/identity until a separate reviewed removal plan; preserve protected state backups |

Mock `apply` runs use provider simulations and create no cloud resources; the helper copies sources privately and overrides only lifecycle deletion protection so the test runner can clean up its simulated state. [OpenTofu tests](https://opentofu.org/docs/cli/commands/test/)
