# Runbook

### Provision

Use the [production OpenTofu inputs and reviewed-plan workflow](platform.md#production-opentofu).

### Restore from backup

| Backend restore | Value |
| --- | --- |
| Service user | `llunde-backend` |
| Runtime directory | `/run/user/2001` |
| Container | `llunde-postgres` |
| Archive | `var/backup/llunde-backend/llunde.dump` |
| Destination | New `llunde_restore` database |
| Cutover | After schema, data, and application verification |

```sh
export RESTIC_REPOSITORY="s3:s3.eu-north-1.amazonaws.com/llunde-pyparser-bucket/<RESTIC_PREFIX>"
restic snapshots
restic restore <SNAPSHOT_ID> --target /tmp/restore
ssh root@<TAILNET_IP> \
  'cd / && runuser -u llunde-backend -- env XDG_RUNTIME_DIR=/run/user/2001 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2001/bus /run/current-system/sw/bin/podman exec llunde-postgres createdb -U llunde llunde_restore'
ssh root@<TAILNET_IP> \
  'cd / && runuser -u llunde-backend -- env XDG_RUNTIME_DIR=/run/user/2001 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2001/bus /run/current-system/sw/bin/podman exec -i llunde-postgres pg_restore --exit-on-error -U llunde -d llunde_restore' \
  < /tmp/restore/var/backup/llunde-backend/llunde.dump
```


### Manual deploy

```sh
ssh root@<TAILNET_IP> systemctl stop gitops-pull.timer   # pause the loop FIRST
nix run nixpkgs#nixos-rebuild -- switch --flake .#<host> \
  --target-host root@<TAILNET_IP> --build-host root@<TAILNET_IP>
# ...restart affected user units per the reconcile map, then:
ssh root@<TAILNET_IP> systemctl start gitops-pull.timer
```

### Install NixOS

```sh
nix run github:nix-community/nixos-anywhere -- \
  --flake .#fredrir-<id> \
  --build-on-remote \
  -i ~/.ssh/id_ed25519 \
  --extra-files ./ssh/admin_keys.csv \ # TODO Fix Syntax of this command
  root@46.62.214.182
```
