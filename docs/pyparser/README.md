# pyparser infrastructure

pyparser runs as rootless podman quadlets under the `pyparser` user on the
**llunde-parser** NixOS host. Its cloud resources — one Hetzner Cloud server plus
**AWS S3/IAM** — are declared in this repo's flat OpenTofu root (`tofu/`), and
Cloudflare fronts it with a tunnel and Access.

| Doc | What |
|---|---|
| [PROD.md](PROD.md) | Operational runbook: topology, secrets, deploys, backups, sizing, restore |
| [CLOUDFLARE.md](CLOUDFLARE.md) | The tunnel + Access configuration, and which parts are in tofu |
| [cf-external-share.sh](cf-external-share.sh) | Idempotent setup of the `external.llunde.no` share origin |

| Tofu file | What |
|---|---|
| `tofu/parser-server.tf` | `module "parser"` (→ `tofu/modules/hetzner`): the CCX23 `llunde-parser` + its rule-less firewall |
| `tofu/parser-aws.tf` | S3 dataset-bucket hardening (block-public-access / versioning / SSE), the `pyparser-dataset` IAM policy + the `leploy` user it is attached to, and the per-host restic IAM users |
| `tofu/cloudflare.tf` + `tofu/modules/cloudflare/` | `llunde.no` zone records, including `parser` and `external` |

## Usage

State is remote and shared with the rest of the estate
(`s3://llunde-pyparser-bucket/tofu-state/infra.tfstate`); AWS credentials come
from the environment or `var.aws_profile`, and one `TF_VAR_hcloud_token` covers
both servers (same Hetzner project):

```sh
export TF_VAR_hcloud_token="<Hetzner Cloud Read & Write token>"   # Console → Security → API Tokens
tofu -chdir=tofu init
tofu -chdir=tofu plan      # steady state: "No changes."
```

Cloudflare work also needs `CLOUDFLARE_API_TOKEN` from Doppler `llunde/ops` —
laptop-only, never on a host ([../cloudflare.md](../cloudflare.md)).

## Adoption note (important)

Every resource here was **created out-of-band and imported** — there is no
apply-creates flow for the server. `module.parser.hcloud_server.this` carries
`prevent_destroy` + `ignore_changes = [image, user_data, ssh_keys, keep_disk,
backups]` plus Hetzner `delete_protection`, so a plan can **never rebuild** the
stateful box (Postgres data lives on a podman named volume on its boot disk —
there is no `hcloud_volume`). Adding a resource means importing the live object
first, then `plan` until clean — never bare-`apply` a fresh server.

AWS resources adopted: the three S3 hardening resources, the `pyparser-dataset`
IAM policy, the `leploy` user + its policy attachment. IAM **access keys** are
deliberately unmanaged — their secrets are unrecoverable, and rotating them
through tofu would break the live credentials on the host.
