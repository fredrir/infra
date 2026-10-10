#!/usr/bin/env bash
set -euo pipefail
tool=$1
"$tool" --version | grep -qx 'gh version 2.102.0 (2026-09-30)'
"$tool" help >/dev/null
