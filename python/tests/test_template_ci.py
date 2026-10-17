import pathlib

from template_ci.cli import load, validate

ROOT = pathlib.Path(__file__).resolve().parents[2] / "templates"


def test_repo_templates_are_valid():
    for d in ROOT.iterdir():
        assert validate(load(d)) == [], d.name


def test_rejects_latest_public_ports_and_bad_licence(tmp_path):
    d = tmp_path / "bad"
    d.mkdir()
    (d / "spade.yml").write_text(
        "slug: bad\nname: Bad\ncategory: x\nlicence_spdx: SSPL-1.0\nversion: '1'\nhealth: {port: 80, path: /}\n")
    (d / "docker-compose.yml").write_text(
        "services:\n  app:\n    image: nginx:latest\n    privileged: true\n    ports: ['80:80']\n")
    errs = validate(load(d))
    assert any("SSPL" in e for e in errs)
    assert any("pinned" in e for e in errs)
    assert any("privileged" in e for e in errs)
    assert any("127.0.0.1" in e for e in errs)
