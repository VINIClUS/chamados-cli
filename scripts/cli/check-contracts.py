#!/usr/bin/env python3
"""Verify the pinned Backend snapshot used by the CLI and its protocol tests."""
import importlib.util
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

def main():
    spec = importlib.util.spec_from_file_location('backend_snapshot', ROOT / 'scripts/update-backend-contracts.py')
    updater = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(updater)
    updater.check(ROOT)
    for name in ('operations-input.schema.json', 'operations-result.schema.json',
                 'executor-manifest.schema.json', 'backup-manifest.schema.json'):
        schema = json.loads((ROOT / 'contracts/backend' / name).read_bytes())
        assert schema['$schema'] == 'https://json-schema.org/draft/2020-12/schema', name
        assert schema['type'] == 'object' and schema['additionalProperties'] is False, name
        assert schema['required'], name
    print('Pinned Backend contracts verified')

if __name__ == '__main__':
    main()
