# Infrastructure (Terraform)

Both production apps run on **separate Hetzner Cloud servers in one Hetzner project**,
managed here as code. Each app is a self-contained Terraform **root module with its
own local state**; they share one reusable server module and one Hetzner API token.

| Path | What |
|---|---|
| `modules/hetzner-server/` | Reusable: adopts a running `hcloud_server` + a network `hcloud_firewall` (inbound rules passed in as a var). `prevent_destroy` + `ignore_changes` so a stateful box can't be rebuilt. |
| `pyparser/` | Root module for the **pyparser** server (parser.llunde.no, Cloudflare tunnel → SSH-only) **+ AWS S3/IAM**. Own state. Docs: `pyparser/PROD.md`, `pyparser/CLOUDFLARE.md`. |
| `llunde/` | Root module for the **llunde.no** server (public nginx/certbot → inbound 22/80/443). Own state, hcloud-only. |

- **Two states, two blast radii**: `terraform` runs per-subdir; a mistake in one can't touch the other. Both servers carry `prevent_destroy` + Hetzner `delete_protection`.
- **One token, same project**: `export TF_VAR_hcloud_token="<Hetzner R&W token>"` satisfies both subdirs.
- **AWS** is pyparser-only (S3 dataset bucket + IAM). **Cloudflare** (shared zone `llunde.no`) is managed out-of-band — see `pyparser/CLOUDFLARE.md`.

## Usage
```sh
export TF_VAR_hcloud_token="<Hetzner Cloud Read & Write token>"
cd infra/pyparser        # or infra/llunde
terraform init
terraform plan           # pyparser steady-state: "No changes."
terraform apply
```

## State backup (local state → S3)
```sh
aws s3 cp infra/pyparser/terraform.tfstate s3://llunde-pyparser-bucket/tf-state-backups/pyparser/terraform.tfstate
aws s3 cp infra/llunde/terraform.tfstate   s3://llunde-pyparser-bucket/tf-state-backups/llunde/terraform.tfstate
```

## Adoption model
Every resource was created out-of-band and `terraform import`ed (no `apply` ever
creates a server). To adopt something new: import the live object, then `plan` until
clean — **never bare-`apply` a fresh server**. The `llunde/server.tf` identity fields
are `TODO` placeholders to fill from `hcloud server describe` before its import. The
`llunde` firewall **must** keep 80 + 443 in its rules before attaching, or the public
site drops — see the no-downtime attach steps below.

### llunde first-time adoption (run locally, where the token + server live)
```sh
export TF_VAR_hcloud_token="<token>"
hcloud server list                                   # fill name/type/location in llunde/server.tf
cd infra/llunde && terraform init
terraform import module.server.hcloud_server.this <SERVER_ID>
terraform plan                                       # server no-op + firewall/attachment to create
terraform apply -target=module.server.hcloud_firewall.this        # create (unattached)
hcloud firewall describe llunde-fw                   # confirm 22/80/443
terraform apply -target=module.server.hcloud_firewall_attachment.this
curl -I https://llunde.no && curl -I http://llunde.no             # verify still up
terraform apply                                      # converge
```
