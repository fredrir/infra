{
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.llunde.gitopsPull;

  stateDir = "/var/lib/gitops-pull";
  textfileDir = "/var/lib/node-exporter-text";

  deadmanScript = pkgs.writeShellApplication {
    name = "gitops-deadman";
    runtimeInputs = [pkgs.curl pkgs.coreutils pkgs.systemd];
    text = ''
      echo "gitops deadman FIRED: a new configuration was activated but never confirmed within ${toString cfg.deadmanMinutes}m — rebooting into the previous boot-default generation"
      ${lib.optionalString (cfg.heartbeatUrlFile != null) ''
        curl -fsS -m 10 --data-raw "gitops deadman fired: rolling back via reboot" \
          "$(cat ${cfg.heartbeatUrlFile})/fail" || true
      ''}
      systemctl reboot || true
      sleep 60
      systemctl reboot --force || true
      sleep 30
      echo b > /proc/sysrq-trigger || true
    '';
  };

  reconcileUnits = lib.flatten (lib.mapAttrsToList (
      user: uCfg:
        lib.mapAttrsToList (unit: unitCfg: {
          inherit user unit;
          inherit (uCfg) uid;
          inherit (unitCfg) watch check;
          key = "${user}/${unit}";
        })
        uCfg.units
    )
    cfg.reconcile);

  preHashSnippets =
    lib.concatMapStringsSep "\n" (u: ''
      PRE["${u.key}"]=$(hash_paths ${lib.escapeShellArgs u.watch})
    '')
    reconcileUnits;

  reconcileSnippets =
    lib.concatMapStringsSep "\n" (u: ''
      if [ "$(hash_paths ${lib.escapeShellArgs u.watch})" != "''${PRE["${u.key}"]}" ]; then
        echo "reconcile: ${u.key} changed across the switch — restarting"
        ${
        if u.check != null
        then ''
          if ${u.check}; then
            user_restart ${lib.escapeShellArgs [u.user (toString u.uid) u.unit]} || RECONCILE_FAILED+=" ${u.key}"
          else
            echo "reconcile: pre-restart check for ${u.key} FAILED — restart skipped, the old container keeps the old config"
            RECONCILE_FAILED+=" ${u.key}(check)"
          fi
        ''
        else ''
          user_restart ${lib.escapeShellArgs [u.user (toString u.uid) u.unit]} || RECONCILE_FAILED+=" ${u.key}"
        ''
      }
      fi
    '')
    reconcileUnits;

  pullScript = pkgs.writeShellApplication {
    name = "gitops-pull";
    runtimeInputs = [
      config.nix.package
      config.services.tailscale.package
      pkgs.git
      pkgs.openssh
      pkgs.curl
      pkgs.jq
      pkgs.iproute2
      pkgs.util-linux # flock, runuser
      pkgs.coreutils
      pkgs.findutils
      pkgs.gnugrep
      pkgs.systemd
    ];
    text = ''
      STATE=${stateDir}
      BRANCH=${lib.escapeShellArg cfg.branch}
      REPO=${lib.escapeShellArg cfg.repoSshUrl}
      FLAKE_BASE=${lib.escapeShellArg cfg.flakeBaseUrl}
      HOST_ATTR=${lib.escapeShellArg cfg.flakeAttr}
      DELAY=${toString cfg.applyDelaySeconds}
      HB_FILE=${
        if cfg.heartbeatUrlFile != null
        then lib.escapeShellArg (toString cfg.heartbeatUrlFile)
        else "\"\""
      }
      PEER=${
        if cfg.probePeer != null
        then lib.escapeShellArg cfg.probePeer
        else "\"\""
      }

      exec 9>>"$STATE/lock"
      flock -n 9 || {
        echo "another gitops-pull run holds the lock — skipping"
        exit 0
      }

      export GIT_SSH_COMMAND="ssh -i ${cfg.sshKeyFile} -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes -o ConnectTimeout=15"

      hb_ok() {
        [ -n "$HB_FILE" ] || return 0
        curl -fsS -m 10 --retry 2 -o /dev/null "$(cat "$HB_FILE")" || true
      }
      # /fail flips the check to down IMMEDIATELY (email) and carries a journal
      # tail as the ping body; the missed-success grace period is the backstop.
      hb_fail() {
        echo "FAIL: $1"
        [ -n "$HB_FILE" ] || return 0
        {
          echo "$1"
          journalctl -u gitops-pull.service -n 40 --no-pager 2>/dev/null | tail -c 8000
        } | curl -fsS -m 10 --data-binary @- -o /dev/null "$(cat "$HB_FILE")/fail" || true
      }

      hash_paths() {
        local acc="" p h
        for p in "$@"; do
          if [ -e "$p" ]; then
            h=$(sha256sum <"$p" | cut -d' ' -f1)
          else
            h="missing"
          fi
          acc="$acc $h"
        done
        printf '%s' "$acc" | sha256sum | cut -d' ' -f1
      }

      # runuser needs a cwd the target user can read (the unit sets /) plus the
      # user-bus env pair; `systemctl --machine` is flaky here non-interactively.
      user_restart() {
        local user="$1" uid="$2" unit="$3"
        runuser -u "$user" -- env "XDG_RUNTIME_DIR=/run/user/$uid" \
          "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$uid/bus" \
          systemctl --user restart "$unit" || return 1
        for _ in 1 2 3 4 5; do
          if runuser -u "$user" -- env "XDG_RUNTIME_DIR=/run/user/$uid" \
            "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$uid/bus" \
            systemctl --user is-active --quiet "$unit"; then
            return 0
          fi
          sleep 2
        done
        echo "reconcile: $unit did not come back active for $user"
        return 1
      }

      probe_once() {
        if ! tailscale status --json 2>/dev/null | jq -e '.Self.Online == true' >/dev/null; then
          if [ -n "$PEER" ]; then
            tailscale ping -c 1 --timeout 5s "$PEER" >/dev/null 2>&1 || return 1
          else
            return 1
          fi
        fi
        ss -ltnH 'sport = :22' | grep -q . || return 1
        timeout 30 git ls-remote "$REPO" HEAD >/dev/null || return 1
        return 0
      }

      disarm_deadman() {
        systemctl stop gitops-deadman.timer 2>/dev/null || true
        systemctl reset-failed gitops-deadman.service gitops-deadman.timer 2>/dev/null || true
      }

      # ---- poll ----
      if ! REV=$(git ls-remote "$REPO" "refs/heads/$BRANCH" | cut -f1) || [ -z "$REV" ]; then
        # Transient (network/GitHub): stay quiet. A persistent inability to poll
        # stops the success heartbeat and the external grace period surfaces it.
        echo "ls-remote for $BRANCH failed — skipping this cycle"
        exit 0
      fi

      APPLIED=$(cat "$STATE/applied" 2>/dev/null || echo none)
      if [ "$REV" = "$APPLIED" ]; then
        if [ -e "$STATE/reconcile_failed" ]; then
          # Sticky: converged on paper, but a unit restart failed after the
          # switch. Clears on the next successful apply, or manually (runbook).
          hb_fail "converged at $REV but an earlier reconcile failed (sticky — see $STATE/reconcile_failed)"
          exit 1
        fi
        hb_ok
        exit 0
      fi

      # ---- failure memory ----
      if [ -e "$STATE/attempting" ]; then
        HELD=$(cat "$STATE/attempting")
        if [ "$HELD" = "$REV" ]; then
          hb_fail "HOLD: rev $REV previously failed to confirm on this host (deadman rollback or interrupted apply) — waiting for a new deploy rev, or: rm $STATE/attempting"
          exit 1
        fi
        echo "clearing stale attempt marker for superseded rev $HELD"
        rm -f "$STATE/attempting"
      fi

      # ---- canary lag ----
      if [ "$DELAY" -gt 0 ]; then
        SEEN="$STATE/first_seen.$REV"
        [ -e "$SEEN" ] || date +%s >"$SEEN"
        AGE=$(($(date +%s) - $(cat "$SEEN")))
        if [ "$AGE" -lt "$DELAY" ]; then
          echo "rev $REV is ''${AGE}s old here; applying after ''${DELAY}s (canary lag)"
          hb_ok
          exit 0
        fi
      fi
      find "$STATE" -name 'first_seen.*' -mmin +10080 -delete 2>/dev/null || true

      # ---- build ----
      FLAKEREF="$FLAKE_BASE?ref=$BRANCH&rev=$REV"
      echo "building $FLAKEREF#$HOST_ATTR"
      if ! OUT=$(nix build --print-out-paths --out-link "$STATE/result" \
        "$FLAKEREF#nixosConfigurations.$HOST_ATTR.config.system.build.toplevel"); then
        N=$(($(cat "$STATE/build_fails" 2>/dev/null || echo 0) + 1))
        echo "$N" >"$STATE/build_fails"
        if [ "$N" -ge 3 ]; then
          hb_fail "build of $REV failed $N consecutive cycles"
          exit 1
        fi
        echo "build failed ($N/3 before alerting) — likely transient, retrying next cycle"
        exit 0
      fi
      rm -f "$STATE/build_fails"

      # ---- pre-switch watch-set hashes (compared after the switch) ----
      declare -A PRE
      ${preHashSnippets}

      # ---- the point of no return ----
      echo "$REV" >"$STATE/attempting"
      sync "$STATE/attempting"
      disarm_deadman
      systemd-run --collect --on-active="${toString cfg.deadmanMinutes}min" \
        --unit=gitops-deadman \
        --description="gitops rollback deadman (reboot into previous generation)" \
        ${deadmanScript}/bin/gitops-deadman
      if ! systemctl is-active --quiet gitops-deadman.timer; then
        rm -f "$STATE/attempting"
        hb_fail "could not arm the rollback deadman — refusing to activate $REV"
        exit 1
      fi

      # ---- activate without touching the bootloader ----
      STC_RC=0
      "$OUT/bin/switch-to-configuration" test || STC_RC=$?
      [ "$STC_RC" -eq 0 ] || echo "switch-to-configuration test exited $STC_RC — probes decide from here (a failed unit is not a severed host)"

      # ---- probes decide ----
      ELAPSED=0
      PASSES=0
      while :; do
        if probe_once; then PASSES=$((PASSES + 1)); else PASSES=0; fi
        [ "$PASSES" -ge 2 ] && break
        if [ "$ELAPSED" -ge 360 ]; then
          hb_fail "reachability probes failed after test-activating $REV — deadman stays armed, reboot+rollback in <=${toString cfg.deadmanMinutes}min"
          exit 1
        fi
        sleep 15
        ELAPSED=$((ELAPSED + 15))
      done

      # ---- confirmed ----
      nix-env -p /nix/var/nix/profiles/system --set "$OUT"
      "$OUT/bin/switch-to-configuration" switch || STC_RC=$?
      disarm_deadman

      GEN=$(readlink /nix/var/nix/profiles/system | grep -o '[0-9]\+' || true)
      if [ -n "$GEN" ] && [ -r /boot/loader/loader.conf ] \
        && ! grep -q "nixos-generation-''${GEN}\.conf" /boot/loader/loader.conf; then
        rm -f "$STATE/attempting"
        hb_fail "switch ran but the boot default is not generation $GEN — system RUNS $REV, boot default may be stale; retrying next cycle"
        exit 1
      fi

      echo "$REV" >"$STATE/applied"
      rm -f "$STATE/attempting" "$STATE/reconcile_failed"
      sync "$STATE/applied"

      # ---- reconcile user quadlets ----
      RECONCILE_FAILED=""
      ${reconcileSnippets}

      mkdir -p ${textfileDir}
      {
        printf 'gitops_pull_last_success_timestamp %s\n' "$(date +%s)"
        printf 'gitops_pull_applied_info{rev="%s"} 1\n' "$REV"
        printf 'gitops_pull_stc_warn %s\n' "$STC_RC"
      } >"${textfileDir}/.gitops_pull.prom.tmp"
      mv "${textfileDir}/.gitops_pull.prom.tmp" "${textfileDir}/gitops_pull.prom"

      if [ -n "$RECONCILE_FAILED" ]; then
        echo "$REV:$RECONCILE_FAILED" >"$STATE/reconcile_failed"
        hb_fail "applied $REV but reconcile failed for:$RECONCILE_FAILED"
        exit 1
      fi

      echo "applied $REV"
      hb_ok
    '';
  };
in {
  options.llunde.gitopsPull = {
    enable = lib.mkOption {
      type = lib.types.bool;
      default = false;
    };
    branch = lib.mkOption {
      type = lib.types.str;
      default = "deploy";
    };
    flakeAttr = lib.mkOption {
      type = lib.types.str;
      default = config.networking.hostName;
    };
    repoSshUrl = lib.mkOption {
      type = lib.types.str;
      default = "git@github.com:fredrir/llunde-infra";
    };
    flakeBaseUrl = lib.mkOption {
      type = lib.types.str;
      default = "git+ssh://git@github.com/fredrir/llunde-infra";
    };
    sshKeyFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
    };
    heartbeatUrlFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
    };
    requireHeartbeat = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Refuse to enable without an external heartbeat (rehearsal opts out).";
    };
    applyDelaySeconds = lib.mkOption {
      type = lib.types.int;
      default = 0;
    };
    deadmanMinutes = lib.mkOption {
      type = lib.types.int;
      default = 10;
    };
    probePeer = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
    };
    interval = lib.mkOption {
      type = lib.types.str;
      default = "*:0/5";
    };
    reconcile = lib.mkOption {
      type = lib.types.attrsOf (lib.types.submodule {
        options = {
          uid = lib.mkOption {
            type = lib.types.int;
          };
          units = lib.mkOption {
            type = lib.types.attrsOf (lib.types.submodule {
              options = {
                watch = lib.mkOption {
                  type = lib.types.listOf lib.types.str;
                };
                check = lib.mkOption {
                  type = lib.types.nullOr lib.types.str;
                  default = null;
                };
              };
            });
            default = {};
          };
        };
      });
      default = {};
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.sshKeyFile != null;
        message = "llunde.gitopsPull needs sshKeyFile (the read-only deploy key).";
      }
      {
        assertion = !cfg.requireHeartbeat || cfg.heartbeatUrlFile != null;
        message = "llunde.gitopsPull: prod hosts must have an external heartbeat (heartbeatUrlFile) — nothing else watches llunde-parser. Rehearsal sets requireHeartbeat = false.";
      }
    ];

    boot.kernel.sysctl."kernel.sysrq" = 176;

    programs.ssh.knownHosts."github.com".publicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl";

    systemd.tmpfiles.rules = ["d ${stateDir} 0700 root root -"];

    systemd.services.gitops-pull = {
      description = "GitOps pull-apply of the gated deploy pointer";
      after = ["network-online.target" "tailscaled.service"];
      wants = ["network-online.target"];
      restartIfChanged = false;
      serviceConfig = {
        Type = "oneshot";
        ExecStart = "${pullScript}/bin/gitops-pull";
        WorkingDirectory = "/";
        MemoryHigh = "1792M";
        MemoryMax = "2304M";
        CPUWeight = 30;
        TimeoutStartSec = 2700;
        Environment = ["HOME=/root"];
      };
      onFailure = ["gitops-pull-failed.service"];
    };

    systemd.services.gitops-pull-failed = lib.mkIf (cfg.heartbeatUrlFile != null) {
      description = "gitops-pull failure notifier";
      serviceConfig.Type = "oneshot";
      script = ''
        {
          echo "gitops-pull.service entered failed state"
          ${pkgs.systemd}/bin/journalctl -u gitops-pull.service -n 40 --no-pager 2>/dev/null | ${pkgs.coreutils}/bin/tail -c 8000
        } | ${pkgs.curl}/bin/curl -fsS -m 10 --data-binary @- -o /dev/null \
          "$(${pkgs.coreutils}/bin/cat ${cfg.heartbeatUrlFile})/fail" || true
      '';
    };

    systemd.timers.gitops-pull = {
      wantedBy = ["timers.target"];
      timerConfig = {
        OnCalendar = cfg.interval;
        RandomizedDelaySec = 30;
        Persistent = false;
      };
    };
  };
}
