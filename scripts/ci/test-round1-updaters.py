#!/usr/bin/env python3
"""Updater contracts use synthetic releases and stub every network/Nix command."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]
SRI = 'sha256-' + 'A' * 43 + '='
STUB = r'''#!/usr/bin/env python3
import json, os, pathlib, sys
name = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
with open('calls', 'a') as log:
    log.write(name + ' ' + ' '.join(args) + '\n')
if name == 'curl':
    if '-o' in args:
        pathlib.Path(args[args.index('-o') + 1]).write_bytes(b'synthetic release')
    elif 'latest-linux.yml' in args[-1]: print('version: 99.1.0')
    elif 'zen-browser' in args[-1]: print('[{"tag_name":"99.1.0","prerelease":false}]')
    else: print('{"tag_name":"v99.1.0"}')
elif name == 'git': print('1234567890\trefs/tags/v99.1.0')
elif name == 'nix-prefetch-url': print('synthetic-hash')
elif name == 'nix':
    if args[:2] == ['hash', 'convert']: print(os.environ['TEST_SRI'])
    elif args[0] == 'eval':
        if os.environ.get('FAIL_STEP') == 'eval': sys.exit(12)
        attr = next(arg for arg in args if arg.startswith('.#'))
        if attr.endswith('.outputHash'):
            data = json.loads(pathlib.Path('pkgs/stash/version.json').read_text())
            field = 'srcHash' if '.src.' in attr else 'pnpmHashV3' if 'V3' in attr else 'pnpmHash'
            print(data[field])
        else: print('/nix/store/synthetic.drv')
    elif args[0] == 'build':
        if '--dry-run' in args: sys.exit(0)
        if '-L' in args:
            if os.environ.get('FAIL_STEP') == 'v3' and any('frontendDepsV3' in arg for arg in args): sys.exit(13)
            print('got: ' + os.environ['TEST_SRI'], file=sys.stderr)
            sys.exit(1)
    else: sys.exit(99)
'''

for tool in ('ledger-live', 'zen-browser', 'stash'):
    for failure in ('none', 'eval') + (('v3',) if tool == 'stash' else ()):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / 'scripts/update-packages').mkdir(parents=True)
            (root / f'pkgs/{tool}').mkdir(parents=True)
            for script in (f'update-{tool}.sh', 'fod-hash.sh'):
                shutil.copy(ROOT / 'scripts/update-packages' / script, root / 'scripts/update-packages' / script)
            if tool == 'stash':
                (root / 'pkgs/stash/version.json').write_text(json.dumps(dict(version='1.0', gitHash='old', srcHash=SRI, pnpmHash='stale-v2', pnpmHashV3='stale-v3', vendorHash='old')))
            else:
                shutil.copy(ROOT / f'pkgs/{tool}/default.nix', root / f'pkgs/{tool}/default.nix')
            (root / 'bin').mkdir()
            for name in ('curl', 'git', 'nix', 'nix-prefetch-url'):
                path = root / 'bin' / name
                path.write_text(STUB)
                path.chmod(0o755)
            output = root / 'output'
            output.touch()
            result = subprocess.run(['bash', f'scripts/update-packages/update-{tool}.sh'], cwd=root, env={'HOME': str(root), 'PATH': str(root / 'bin') + ':' + os.environ['PATH'], 'GITHUB_OUTPUT': str(output), 'TEST_SRI': SRI, 'FAIL_STEP': failure}, capture_output=True, text=True)
            assert (result.returncode == 0) == (failure == 'none'), (tool, failure, result.stdout, result.stderr)
            assert ('updated=true' in output.read_text()) == (failure == 'none'), (tool, failure)
            calls = (root / 'calls').read_text()
            if tool == 'stash' and failure == 'none':
                data = json.loads((root / 'pkgs/stash/version.json').read_text())
                assert data['pnpmHash'] == SRI and data['pnpmHashV3'] == SRI
                for fetcher in ('frontendDepsV2', 'frontendDepsV3'):
                    assert f'build -L --no-link .#stash.{fetcher}' in calls
                    assert f'build --no-link .#stash.{fetcher}' in calls
            if tool != 'stash':
                assert 'nix build' not in calls
                assert f'nix eval --raw .#packages.x86_64-linux.{tool}.drvPath' in calls
print('Round 1 updater contracts passed.')
