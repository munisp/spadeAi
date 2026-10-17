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


def test_rejects_host_mounts_and_namespace_sharing(tmp_path):
    d = tmp_path / "escape"
    d.mkdir()
    (d / "spade.yml").write_text(
        "slug: escape\nname: E\ncategory: x\nlicence_spdx: MIT\nversion: '1'\nhealth: {port: 80, path: /}\n")
    (d / "docker-compose.yml").write_text(
        "services:\n  app:\n    image: nginx:1.27\n    network_mode: host\n    cap_add: [SYS_ADMIN]\n"
        "    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n      - ./data:/data\n"
        "      - {type: bind, source: /etc, target: /host-etc}\n      - data:/ok\n      - etc/x:/rel\n"
        "      - undeclared:/u\n      - /anon\n"
        "volumes:\n  data: {}\n  sneaky: {driver_opts: {type: none, o: bind, device: /}}\n")
    errs = validate(load(d))
    assert sum("bind" in e for e in errs) >= 5, errs
    assert any("etc/x" in e for e in errs) and any("undeclared" in e for e in errs)
    assert not any("/anon" in e for e in errs)
    assert any("network_mode" in e for e in errs)
    assert any("cap_add" in e for e in errs)
    assert any("sneaky" in e for e in errs)
    assert not any("data:/ok" in e for e in errs)
