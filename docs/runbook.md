# Runbook

### Provision

```sh
doppler run --project pyparser --config prd -- tofu -chdir=tofu apply
```

### Restore from backup

```sh
export RESTIC_REPOSITORY="s3:s3.eu-north-1.amazonaws.com/llunde-pyparser-bucket/<RESTIC_PREFIX>"   
restic snapshots   
restic restore latest --target /tmp/restore
ssh root@<TAILNET_IP> "podman exec -i -u postgres llunde-postgres psql -U llunde llunde" < /tmp/restore/<DUMP_PATH> 
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
