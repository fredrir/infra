import copy
import json
from pathlib import Path
import sys
import unittest

from jsonschema import Draft7Validator
import yaml

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'tools'))

from infra.contracts import ContractError, validate_project
from infra.render import chart_values, render_project


def project():
    value = yaml.safe_load((ROOT / 'tests/infra/fixtures/project.yaml').read_text())
    value['migration'] = {'component': 'app', 'command': ['alembic'], 'args': ['upgrade', 'head'], 'compatibility': 'backward-compatible', 'retrySafe': True, 'secretKeys': ['DATABASE_URL']}
    return value


def release():
    return json.loads((ROOT / 'tests/infra/fixtures/release.json').read_text())


class MigrationTests(unittest.TestCase):
    def test_data_and_migration_require_a_release(self):
        resources = render_project(ROOT, project())
        self.assertFalse(any(item['kind'] == 'HelmRelease' for item in resources))
        accounts = {item['metadata']['name']: item for item in resources if item['kind'] == 'ServiceAccount'}
        self.assertFalse(accounts['project-migration']['automountServiceAccountToken'])
        self.assertFalse(accounts['project-data']['automountServiceAccountToken'])
        headers = next(item for item in resources if item['kind'] == 'Middleware')
        self.assertEqual(headers['spec']['headers']['customRequestHeaders']['X-Admin-Origin'], '')
        roles = [item for item in resources if item['kind'] == 'Role']
        self.assertFalse(any('middlewares' in rule['resources'] for role in roles for rule in role['rules']))

    def test_application_waits_for_separately_owned_data_without_data_rollback(self):
        resources = render_project(ROOT, project(), release())
        releases = {item['metadata']['name']: item for item in resources if item['kind'] == 'HelmRelease'}
        application, data = releases['llunde-backend'], releases['llunde-backend-data']
        self.assertEqual(application['spec']['dependsOn'], [{'name': 'llunde-backend-data'}])
        self.assertNotIn('data', application['spec']['values'])
        self.assertEqual(data['spec']['values']['project'], 'llunde-backend')
        self.assertEqual(data['spec']['serviceAccountName'], 'project-data-reconciler')
        self.assertEqual(data['spec']['chart']['spec']['chart'], './charts/project-data')
        for action in ['install', 'upgrade']:
            self.assertEqual(data['spec'][action]['strategy']['name'], 'RetryOnFailure')
            self.assertNotIn('remediation', data['spec'][action])
        self.assertTrue(application['spec']['rollback']['disableHooks'])

    def test_migration_uses_verified_image_and_named_secrets(self):
        resources = render_project(ROOT, project(), release())
        application = next(item for item in resources if item['kind'] == 'HelmRelease' and item['metadata']['name'] == 'llunde-backend')
        migration = application['spec']['values']['migration']
        self.assertEqual(migration['image'], release()['image'])
        self.assertEqual(migration['sourceRevision'], release()['sourceRevision'])
        self.assertEqual(migration['serviceAccountName'], 'project-migration')
        secret = next(item for item in resources if item['kind'] == 'ExternalSecret' and item['metadata']['name'] == 'project-runtime')
        self.assertEqual(sum(item['secretKey'] == 'DATABASE_URL' for item in secret['spec']['data']), 1)

    def test_migration_component_must_share_source_and_workflow_run(self):
        value = {'schemaVersion': 1, 'project': 'portfolio', 'workloads': {'web': {'kind': 'web', 'component': 'web', 'port': 3000, 'healthPath': '/'}}, 'data': {'postgres': {'sizeClass': 'small', 'recovery': {'rpoHours': 24, 'rtoHours': 4}}}, 'migration': {'component': 'api', 'command': ['alembic'], 'args': ['upgrade', 'head'], 'compatibility': 'backward-compatible', 'retrySafe': True}}
        record = release()
        record.update(project='portfolio', component='web', image='ghcr.io/fredrir/portfolio-web@sha256:' + 'a' * 64)
        record['provenance']['repositoryId'] = 773018612
        migration = copy.deepcopy(record)
        migration.update(component='api', image='ghcr.io/fredrir/portfolio-api@sha256:' + 'c' * 64)
        with self.assertRaisesRegex(ContractError, 'component release required'):
            chart_values(ROOT, value, releases={'web': record})
        compiled = chart_values(ROOT, value, releases={'web': record, 'api': migration})
        self.assertEqual(compiled['migration']['image'], migration['image'])
        migration['provenance']['runId'] += 1
        with self.assertRaisesRegex(ContractError, 'same source'):
            chart_values(ROOT, value, releases={'web': record, 'api': migration})

    def test_destructive_or_arbitrary_pod_contracts_are_rejected(self):
        for change in [{'compatibility': 'destructive'}, {'retrySafe': False}, {'volumes': []}, {'serviceAccountName': 'project-reconciler'}]:
            value = project()
            value['migration'].update(change)
            with self.assertRaises(ContractError):
                validate_project(ROOT, value)
        value = project()
        value.pop('data')
        with self.assertRaises(ContractError):
            validate_project(ROOT, value)
        value = project()
        value['migration'].pop('retrySafe')
        with self.assertRaises(ContractError):
            validate_project(ROOT, value)
        value = project()
        value['workloads']['migration'] = {'kind': 'worker'}
        with self.assertRaisesRegex(ContractError, 'reserved workload name'):
            validate_project(ROOT, value)

    def test_data_chart_refuses_unapproved_images_and_unbounded_storage(self):
        value = yaml.safe_load((ROOT / 'tests/infra/fixtures/data-values.yaml').read_text())
        validator = Draft7Validator(json.loads((ROOT / 'charts/project-data/values.schema.json').read_text()))
        validator.validate(value)
        value['data']['postgres']['image'] = 'postgres:latest'
        self.assertFalse(validator.is_valid(value))
        value = yaml.safe_load((ROOT / 'tests/infra/fixtures/data-values.yaml').read_text())
        value['data']['postgres']['size'] = '1000Gi'
        self.assertFalse(validator.is_valid(value))


if __name__ == '__main__':
    unittest.main()
