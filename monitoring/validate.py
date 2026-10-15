#!/usr/bin/env python3
"""Validate the monitoring bundle's machine-readable files.

Checks:
  - grafana/dashboards/*.json parse as JSON, every panel has a title, a type
    and a unique id, and every panel that carries targets has a non-empty expr.
  - prometheus/rules/*.yml parse as YAML, every group has a name and rules,
    and every alert rule has an expr and a severity label (for: is optional:
    a windowed expr like resets() over 1h needs no extra for).

Run from the monitoring directory (python3 validate.py) or from anywhere with
an explicit path (python3 monitoring/validate.py path/to/monitoring).
Exits non-zero on the first failure, printing every problem found.
"""
import json
import sys
from pathlib import Path

try:
    import yaml
except ImportError:
    yaml = None


def check_dashboards(root: Path) -> list:
    problems = []
    for path in sorted(root.glob("grafana/dashboards/*.json")):
        try:
            d = json.loads(path.read_text())
        except Exception as e:
            problems.append(f"{path}: not valid JSON: {e}")
            continue
        ids = set()
        panels = d.get("panels") or []
        if not panels:
            problems.append(f"{path}: no panels")
        for i, p in enumerate(panels):
            where = f"{path}: panel[{i}]"
            if not p.get("title"):
                problems.append(f"{where}: missing title")
            if not p.get("type"):
                problems.append(f"{where}: missing type")
            pid = p.get("id")
            if pid is None:
                problems.append(f"{where}: missing id")
            elif pid in ids:
                problems.append(f"{where}: duplicate id {pid}")
            else:
                ids.add(pid)
            for t in p.get("targets") or []:
                if not (t.get("expr") or "").strip():
                    problems.append(f"{where}: target {t.get('refId')} has an empty expr")
    return problems


def check_rules_fallback(path: Path) -> list:
    """Structural scan used when PyYAML is unavailable: the rules files are
    plain enough that alert blocks can be checked by their own indentation."""
    problems = []
    lines = path.read_text().splitlines()
    current = None
    in_alert = False
    has_expr = has_for = has_sev = False

    def close_alert():
        nonlocal has_expr, has_for, has_sev
        if in_alert:
            if not has_expr:
                problems.append(f"{path}: {current}: no expr")
            if not has_sev:
                problems.append(f"{path}: {current}: no severity label")
        has_expr = has_for = has_sev = False

    for line in lines:
        stripped = line.strip()
        if stripped.startswith("- alert:"):
            close_alert()
            in_alert = True
            current = stripped[len("- alert:"):].strip()
        elif in_alert:
            if stripped.startswith("#"):
                continue
            if "expr:" in stripped:
                has_expr = True
            if line.strip().startswith("for:"):
                has_for = True
            if stripped.startswith("severity:"):
                has_sev = True
            if line and not line[0].isspace() and not stripped.startswith("- "):
                close_alert()
                in_alert = False
    close_alert()
    return problems


def check_rules(root: Path) -> list:
    problems = []
    if yaml is None:
        for path in sorted(root.glob("prometheus/rules/*.yml")):
            problems.extend(check_rules_fallback(path))
        print("note: PyYAML is not installed; rules were checked structurally")
        return problems
    for path in sorted(root.glob("prometheus/rules/*.yml")):
        try:
            d = yaml.safe_load(path.read_text())
        except Exception as e:
            problems.append(f"{path}: not valid YAML: {e}")
            continue
        for group in (d or {}).get("groups") or []:
            if not group.get("name"):
                problems.append(f"{path}: a group has no name")
            rules = group.get("rules") or []
            if not rules:
                problems.append(f"{path}: group {group.get('name')} has no rules")
            for r in rules:
                if "alert" in r:
                    where = f"{path}: {group.get('name')}/{r.get('alert')}"
                    if not (r.get("expr") or "").strip():
                        problems.append(f"{where}: no expr")
                    if not (r.get("labels") or {}).get("severity"):
                        problems.append(f"{where}: no severity label")
    return problems


def main() -> int:
    root = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(__file__).resolve().parent
    problems = check_dashboards(root) + check_rules(root)
    for p in problems:
        print("PROBLEM:", p)
    if problems:
        print(f"{len(problems)} problem(s)")
        return 1
    print("monitoring bundle validates: dashboards and rules are consistent")
    return 0


if __name__ == "__main__":
    sys.exit(main())
