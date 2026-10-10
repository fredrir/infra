#!/usr/bin/env bash
set -euo pipefail
/home/runner/run.sh --version | grep -x 2.337.0
kubectl version --client
kustomize version
helm version --short
actionlint -version
tofu version
ansible-playbook --version | head -n1
sops --version
age --version
/opt/venv/bin/python -c 'import ansible, jinja2, yaml'
rm -rf /home/runner/_diag /home/runner/run-helper.sh /root/.terraform.d /root/.ansible
