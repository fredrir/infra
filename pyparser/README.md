# pyparser infrastructure

Terraform root module for the pyparser production stack: one **Hetzner Cloud** server
+ **AWS S3/IAM**. Part of the repo-wide `infra/` (see `../README.md`); the server is
defined via the shared `../modules/hetzner-server` module. **Cloudflare** fronts it but
is managed *outside* Terraform — see `CLOUDFLARE.md`.

| File | What |
|---|---|
| `server.tf` | `module "server"` (→ `../modules/hetzner-server`): the CCX23 `llunde-parser` + an SSH-only firewall. |
| `moved.tf` | Rename blocks that wrapped the old root-level hcloud resources into `module.server` (safe to delete after the first clean apply). |
| `main.tf` | S3 dataset-bucket hardening (block-public-access / versioning / SSE) + the `pyparser-dataset` IAM policy + the `leploy` user it's attached to. |
| `versions.tf` | Provider pins (`aws`, `hcloud`) + provider config. Local state (commented S3 backend for later). |
| `variables.tf` / `outputs.tf` | Inputs (`region`, `dataset_bucket_name`, `hcloud_token`) + outputs (bucket, policy ARN, server IPv4). |
| `PROD.md` | Hetzner operational runbook (deploy, backups, sizing, restore). |
| `CLOUDFLARE.md` | The Cloudflare tunnel + Access config and why it's *not* in Terraform. |

The prod Docker Compose stack lives at `../../pyparser/docker-compose.prod.yml`
and is shipped to `/opt/pyparser/docker-compose.yml` on the host by the `Deploy
pyparser` workflow on every deploy (no longer hand-copied); local dev uses
`../../pyparser/docker-compose.yml`.

## Usage

State is a **local** `terraform.tfstate` (gitignored; back up to
`s3://llunde-pyparser-bucket/tf-state-backups/pyparser/`). One `TF_VAR_hcloud_token`
covers both this and the `llunde` module (same Hetzner project — see `../README.md`):

```sh
export TF_VAR_hcloud_token="<Hetzner Cloud Read & Write token>"   # Console → Security → API Tokens
cd infra/pyparser
terraform init
terraform plan      # first run: 3 moves, then "No changes."
terraform apply
```

## Adoption note (important)

Every resource here was **created out-of-band and `terraform import`ed** — there is no
apply-creates flow for the server. `module.server.hcloud_server.this` carries
`prevent_destroy` + `ignore_changes = [image, user_data, ssh_keys, keep_disk, backups]`
plus Hetzner `delete_protection`, so a plan can **never rebuild** the stateful box
(Postgres data lives on a Docker named volume on its boot disk — there is no
`hcloud_volume`). Add a resource? Import the live object first, then `plan` until clean
— never bare-`apply` a fresh server.

AWS resources adopted: the three S3 hardening resources, the `pyparser-dataset` IAM
policy, the `leploy` user + its policy attachment. The IAM **access key** is
deliberately unmanaged (its secret is unrecoverable; the host `secrets.env` holds it).
Cloudflare is unmanaged by design — see `CLOUDFLARE.md`.
