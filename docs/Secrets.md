# Secrets

| File                         | Keys                          | Host          |
| ---------------------------- | ----------------------------- | ------------- |
| `doppler.yaml`               | `doppler_token`               | llunde-01     |
| `pyparser-doppler.yaml`      | `doppler_token`               | llunde-parser |
| `tailscale.yaml`             | `auth_key`                    | **both**      |
| `ghcr.yaml`                  | `auth_json`                   | **both**      |
| `restic.yaml`                | `password`, `env`             | llunde-01     |
| `pyparser-restic.yaml`       | `password`, `env`             | llunde-parser |
| `llunde-backend-db.yaml`     | `env`                         | llunde-01     |
| `llunde-tunnel.yaml`         | `env`                         | llunde-01     |
| `llunde-caddy-acme.yaml`     | `env`                         | llunde-01     |
| `llunde-caddy-ghcr.yaml`     | `auth_json`                   | llunde-01     |
| `observability-smtp.yaml`    | `env`                         | llunde-parser |
| `observability-grafana.yaml` | `env`                         | llunde-parser |
| `gitops-llunde-01.yaml`      | `deploy_key`, `heartbeat_url` | llunde-01     |
| `gitops-llunde-parser.yaml`  | `deploy_key`, `heartbeat_url` | llunde-parser |