from __future__ import annotations

import argparse
import concurrent.futures
import hashlib
import json
from pathlib import Path
import subprocess
import tempfile
import urllib.request

import yaml


class Loader(yaml.SafeLoader):
    pass


Loader.add_constructor('tag:yaml.org,2002:value', lambda loader, node: loader.construct_scalar(node))


def command(args: list[str]) -> str:
    result = subprocess.run(args, check=False, capture_output=True, text=True)
    if result.returncode:
        raise ValueError(result.stderr or result.stdout)
    return result.stdout + (result.stderr if args[1] == 'pull' else '')


def convert_schema(value):
    if isinstance(value, list):
        return [convert_schema(item) for item in value]
    if not isinstance(value, dict):
        return value
    result = {key: convert_schema(item) for key, item in value.items()}
    if result.get('x-kubernetes-int-or-string') is True and 'anyOf' not in result:
        result['anyOf'] = [{'type': 'integer'}, {'type': 'string'}]
    if result.get('nullable') is True and 'type' in result:
        result['type'] = [result['type'], 'null']
    if result.get('type') == 'object' and 'properties' in result and not result.get('x-kubernetes-preserve-unknown-fields'):
        result.setdefault('additionalProperties', False)
    return result


def validate_migration_render(application, data, values):
    jobs = [item for item in application if item and item['kind'] == 'Job']
    if len(jobs) != 1:
        raise ValueError('Migration fixture must render one blocking Helm hook')
    job = jobs[0]
    if job['metadata']['annotations'].get('helm.sh/hook') != 'pre-install,pre-upgrade' or job['spec'].get('backoffLimit') != 0 or job['spec'].get('activeDeadlineSeconds') != 300:
        raise ValueError('Migration must run before application changes with bounded execution')
    pod = job['spec']['template']
    if pod['spec'].get('serviceAccountName') != 'project-migration' or pod['spec'].get('automountServiceAccountToken') is not False:
        raise ValueError('Migration must use its tokenless platform-owned account')
    if any('persistentVolumeClaim' in item or 'hostPath' in item for item in pod['spec'].get('volumes', [])):
        raise ValueError('Migration may not mount application or host data')
    if pod['spec']['containers'][0]['image'] != values['migration']['image']:
        raise ValueError('Migration image differs from its verified component release')
    if any(item and item['kind'] == 'StatefulSet' for item in application):
        raise ValueError('Application rollback may not own database resources')
    databases = [item for item in data if item and item['kind'] == 'StatefulSet']
    for item in databases:
        component = item['spec']['template']['metadata']['labels']['app.kubernetes.io/component']
        if item['metadata']['name'] != values['project'] + '-' + component or item['spec']['template']['spec']['serviceAccountName'] != 'project-data':
            raise ValueError('Data release must preserve project resource identity and its own account')
    policies = [item for item in data if item and item['kind'] == 'NetworkPolicy']
    labels = pod['metadata']['labels']
    def selects(selector):
        return all(labels.get(key) == value for key, value in selector.get('matchLabels', {}).items())
    ingress = any(any(selects(peer.get('podSelector', {})) for peer in rule.get('from', [])) for item in policies for rule in item['spec'].get('ingress', []) if any(port.get('port') == 5432 for port in rule.get('ports', [])))
    egress = any(selects(item['spec']['podSelector']) and any(port.get('port') == 5432 for rule in item['spec'].get('egress', []) for port in rule.get('ports', [])) for item in policies)
    if not ingress or not egress:
        raise ValueError('Data release must allow migration access to PostgreSQL before application installation')


def validate(root: Path, cache: Path, helm='helm', kubectl='kubectl', kubeconform='kubeconform') -> str:
    versions = yaml.safe_load((root / 'platform/versions.yaml').read_text())
    cache.mkdir(parents=True, exist_ok=True)
    releases = []
    project_releases = []
    compiled = []
    for path in sorted((root / 'platform/components').iterdir()):
        if (path / 'kustomization.yaml').is_file():
            compiled.extend(yaml.load_all(command([kubectl, 'kustomize', str(path)]), Loader=Loader))
    compiled.extend(yaml.load_all(command([kubectl, 'kustomize', str(root / 'platform/clusters/production')]), Loader=Loader))
    if (root / 'platform/projects/kustomization.yaml').is_file():
        compiled.extend(yaml.load_all(command([kubectl, 'kustomize', str(root / 'platform/projects')]), Loader=Loader))
    for item in compiled:
        if item and item['kind'] == 'HelmRelease' and item['spec']['chart']['spec']['chart'] in versions['charts']:
            releases.append(item)
        elif item and item['kind'] == 'HelmRelease':
            chart = item['spec']['chart']['spec']
            if chart['chart'] not in {'./charts/project', './charts/project-data'} or chart['sourceRef'] != {'kind': 'GitRepository', 'name': 'infra', 'namespace': 'flux-system'}:
                raise ValueError('Unapproved project Helm source')
            project_releases.append(item)
    needed = {item['spec']['chart']['spec']['chart'] for item in releases}

    def pull(chart):
        pin = versions['charts'][chart]
        archive = cache / f"{chart}-{pin['version']}.tgz"
        if pin['repository'].startswith('oci:'):
            output = command([helm, 'pull', pin['repository'] + '/' + chart, '--version', pin['version'], '--destination', str(cache)])
            if pin['ociDigest'] not in output:
                raise ValueError(f'OCI chart digest drift: {chart}')
        elif not archive.is_file():
            command([helm, 'pull', chart, '--repo', pin['repository'], '--version', pin['version'], '--destination', str(cache)])
        if 'sha256' in pin and hashlib.sha256(archive.read_bytes()).hexdigest() != pin['sha256']:
            raise ValueError(f'Chart archive digest drift: {chart}')
        return chart, archive

    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        archives = dict(pool.map(pull, sorted(needed)))
    chart_resources = []
    for index, item in enumerate(releases):
        values = cache / f'values-{index}.yaml'
        values.write_text(yaml.safe_dump(item['spec']['values']))
        chart = item['spec']['chart']['spec']['chart']
        rendered = command([helm, 'template', item['metadata']['name'], str(archives[chart]), '--namespace', item['metadata']['namespace'], '--kube-version', versions['kubernetes'], '--include-crds', '-f', str(values)])
        chart_resources.extend(yaml.load_all(rendered, Loader=Loader))
    for index, item in enumerate(project_releases):
        values = cache / f'project-values-{index}.yaml'
        values.write_text(yaml.safe_dump(item['spec']['values']))
        chart = item['spec']['chart']['spec']['chart'].removeprefix('./')
        rendered = command([helm, 'template', item['spec'].get('releaseName', item['metadata']['name']), str(root / chart), '--namespace', item['metadata']['namespace'], '--kube-version', versions['kubernetes'], '-f', str(values)])
        chart_resources.extend(yaml.load_all(rendered, Loader=Loader))
    fixture = root / 'tests/infra/fixtures/values.yaml'
    fixture_values = yaml.safe_load(fixture.read_text())
    rendered = command([helm, 'template', fixture_values['project'], str(root / 'charts/project'), '--namespace', 'validation-fixture', '--kube-version', versions['kubernetes'], '-f', str(fixture)])
    application_fixture = list(yaml.load_all(rendered, Loader=Loader))
    chart_resources.extend(application_fixture)
    fixture = root / 'tests/infra/fixtures/data-values.yaml'
    rendered = command([helm, 'template', fixture_values['project'] + '-data', str(root / 'charts/project-data'), '--namespace', 'validation-fixture', '--kube-version', versions['kubernetes'], '-f', str(fixture)])
    data_fixture = list(yaml.load_all(rendered, Loader=Loader))
    chart_resources.extend(data_fixture)
    validate_migration_render(application_fixture, data_fixture, fixture_values)
    pin = versions['fluxManifest']
    flux_bytes = urllib.request.urlopen(pin['url'], timeout=60).read()
    if hashlib.sha256(flux_bytes).hexdigest() != pin['sha256']:
        raise ValueError('Flux installation manifest digest drift')
    schema_sources = list(yaml.load_all(flux_bytes, Loader=Loader)) + chart_resources
    schemas = cache / 'schemas'
    pin = versions['crdSchema']
    crd_bytes = urllib.request.urlopen(pin['url'], timeout=60).read()
    if hashlib.sha256(crd_bytes).hexdigest() != pin['sha256']:
        raise ValueError('Kubernetes CRD schema digest drift')
    crd_document = convert_schema(json.loads(crd_bytes))
    crd_schema = {'$schema': 'http://json-schema.org/draft-04/schema#', **crd_document['components']['schemas']['io.k8s.apiextensions-apiserver.pkg.apis.apiextensions.v1.CustomResourceDefinition'], 'definitions': crd_document['components']['schemas']}
    group = schemas / 'apiextensions.k8s.io'
    group.mkdir(parents=True, exist_ok=True)
    (group / 'customresourcedefinition_v1.json').write_text(json.dumps(crd_schema).replace('#/components/schemas/', '#/definitions/'))
    for item in schema_sources:
        if item and item['kind'] == 'CustomResourceDefinition':
            for version in item['spec']['versions']:
                schema = convert_schema(version['schema']['openAPIV3Schema'])
                group = schemas / item['spec']['group']
                group.mkdir(parents=True, exist_ok=True)
                path = group / f"{item['spec']['names']['kind'].lower()}_{version['name']}.json"
                path.write_text(json.dumps(schema))
    manifest = cache / 'rendered.yaml'
    manifest.write_text(yaml.safe_dump_all([item for item in compiled + chart_resources if item], sort_keys=False))
    return command([kubeconform, '-strict', '-summary', '-kubernetes-version', versions['kubernetes'], '-schema-location', str(schemas / '{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'), '-schema-location', 'default', str(manifest)])


def main(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument('--root', type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument('--cache', type=Path)
    parser.add_argument('--helm', default='helm')
    parser.add_argument('--kubectl', default='kubectl')
    parser.add_argument('--kubeconform', default='kubeconform')
    args = parser.parse_args(argv)
    with tempfile.TemporaryDirectory(prefix='infra-platform-validate-') as temporary:
        print(validate(args.root, args.cache or Path(temporary), args.helm, args.kubectl, args.kubeconform), end='')


if __name__ == '__main__':
    main()
