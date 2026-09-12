import argparse
import json
from pathlib import Path
import yaml


class Dumper(yaml.SafeDumper):
    def ignore_aliases(self, value):
        return True


Dumper.add_representer(str, lambda dumper, value: dumper.represent_scalar('tag:yaml.org,2002:str', value, style='|' if '\n' in value else None))


def substitute(value, replacements):
    if isinstance(value, dict):
        return {key: substitute(item, replacements) for key, item in value.items()}
    if isinstance(value, list):
        return [substitute(item, replacements) for item in value]
    if isinstance(value, str):
        for key, replacement in replacements.items():
            value = value.replace('${' + key + '}', replacement)
    return value


def render(root):
    catalog = json.loads((root / 'platform/catalog/projects.json').read_text())
    versions = yaml.safe_load((root / 'platform/versions.yaml').read_text())
    template = list(yaml.safe_load_all((root / 'platform/components/runners/scale-set.yaml.in').read_text()))
    resources, seen = [], set()
    for project in catalog['projects'].values():
        for architecture in project['architectures']:
            identity = (project['repositoryId'], architecture)
            if identity in seen:
                continue
            seen.add(identity)
            replacements = {'RUNNER_NAMESPACE': f'ci-{identity[0]}-{architecture}', 'RUNNER_NAME': f'infra-{identity[0]}-{architecture}', 'REPOSITORY_URL': 'https://github.com/' + project['repository'], 'RUNNER_ARCH': architecture, 'RUNNER_IMAGE': versions['images']['runner']}
            items = substitute(template, replacements)
            for item in items:
                item['metadata'].setdefault('labels', {})['infra.fredrir.com/repository-id'] = str(identity[0])
                if item['kind'] == 'HelmRelease':
                    item['spec']['chart']['spec']['version'] = versions['charts']['gha-runner-scale-set']['version']
            resources.extend(items)
    return yaml.dump_all(resources, Dumper=Dumper, sort_keys=False, width=110)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--root', type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument('--write', action='store_true')
    args = parser.parse_args()
    result = render(args.root)
    destination = args.root / 'platform/components/runners/scalesets.yaml'
    if args.write:
        destination.write_text(result)
    elif destination.read_text() != result:
        raise SystemExit('Runner resources differ from the approved catalog and template')
    else:
        print('Runner generation is consistent')


if __name__ == '__main__':
    main()
