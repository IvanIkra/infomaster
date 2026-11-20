"""The web app's own scripts (§8.6) parse: a syntax error in one module leaves the whole app blank, and nothing else
in the tests runs them. Needs Node; skipped without it."""

import shutil
import subprocess
from pathlib import Path

import pytest

STATIC = Path(__file__).parent.parent / "infomaster" / "web" / "static"
NODE = shutil.which("node")


@pytest.mark.skipif(NODE is None, reason="no Node to parse the scripts")
@pytest.mark.parametrize("script", sorted(p.name for p in STATIC.glob("*.js")))
def test_script_parses(script, tmp_path):
    module = tmp_path / (Path(script).stem + ".mjs")  # the app's scripts are ES modules
    module.write_text((STATIC / script).read_text())
    found = subprocess.run([NODE, "--check", str(module)], capture_output=True, text=True)
    assert found.returncode == 0, found.stderr
