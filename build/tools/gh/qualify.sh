#!/usr/bin/env bash
set -euo pipefail
tool=$1
"$tool" --version | grep -qx 'gh version 2.103.3 (2026-10-26)'
"$tool" help >/dev/null
