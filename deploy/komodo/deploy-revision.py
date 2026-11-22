#!/usr/bin/env python3
"""Deploy one hub image revision to the Komodo stack and wait until it is healthy.

Usage: deploy-revision.py <stack> <revision>

The image tag is the line HUB_BUILD_REVISION=<revision> in the stack
environment. This script rewrites that one line (UpdateStack), runs DeployStack,
waits for the Komodo update to finish, and then waits until the hub container
runs the new tag and reports healthy. Postgres keeps its own image, so this
script does not require that container to use the hub tag. The other
environment lines only reference secret Variables, so the script never handles
a secret. A Komodo Variable is not used for the tag, because only an admin may
write Variables and the deploy key belongs to a service user.

The stack environment must already contain exactly one HUB_BUILD_REVISION line.
Create that line when you create the stack. This script does not add it.

Environment: KOMODO_URL (default http://localhost:9120), KOMODO_API_KEY and
KOMODO_API_SECRET of a Komodo service user with Write on the stack. It runs on
the Komodo host, because the API listens there only.
"""
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

URL = os.environ.get("KOMODO_URL", "http://localhost:9120").rstrip("/")
DEPLOY_TIMEOUT_S = 1200
HEALTH_TIMEOUT_S = 300
HUB_SERVICE = "hub"
HUB_IMAGE = "git-event-hub"
REVISION_LINE = re.compile(r"(?m)^HUB_BUILD_REVISION=.*$")


def rewrite_revision(env: str, revision: str) -> str:
    """Replace the one HUB_BUILD_REVISION line. Raise ValueError on a bad input."""
    if not re.fullmatch(r"[0-9a-f]{7,40}", revision):
        raise ValueError("revision must be 7 to 40 hex characters")
    updated, count = REVISION_LINE.subn(f"HUB_BUILD_REVISION={revision}", env)
    if count != 1:
        raise ValueError(f"expected one HUB_BUILD_REVISION line, found {count}")
    return updated


def image_has_revision(image: str, revision: str) -> bool:
    """Report whether the image is the hub image with this exact tag."""
    name, sep, tag = image.rpartition(":")
    return sep == ":" and tag == revision and name.endswith("/" + HUB_IMAGE)


def hub_ready(rows: list[dict], revision: str) -> bool:
    """Report whether every listed hub container is healthy on this tag."""
    return bool(rows) and all(
        image_has_revision(r["Image"], revision) and r["Health"] == "healthy" for r in rows
    )


def kapi(endpoint: str, typ: str, params: dict) -> dict:
    req = urllib.request.Request(
        f"{URL}/{endpoint}",
        data=json.dumps({"type": typ, "params": params}).encode(),
        headers={
            "content-type": "application/json",
            "x-api-key": os.environ["KOMODO_API_KEY"],
            "x-api-secret": os.environ["KOMODO_API_SECRET"],
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            return json.load(resp)
    except urllib.error.HTTPError as exc:
        # The body can echo the stack environment. Print only the status.
        print(f"Komodo {typ} failed: HTTP {exc.code}", file=sys.stderr)
        raise SystemExit(1) from None


def redact(text: str) -> str:
    """Hide values of KEY=value lines that carry a secret. The key stays visible."""
    kept = []
    for line in text.splitlines():
        if "=" not in line:
            kept.append(line)
            continue
        key, _value = line.split("=", 1)
        upper = key.strip().upper()
        if any(part in upper for part in ("SECRET", "PASSWORD", "TOKEN", "DATABASE_URL")):
            kept.append(f"{key}=<redacted>")
        else:
            kept.append(line)
    return "\n".join(kept)


def wait_update(update: dict) -> bool:
    uid = update["_id"]["$oid"]
    deadline = time.time() + DEPLOY_TIMEOUT_S
    while True:
        u = kapi("read", "GetUpdate", {"id": uid})
        if u.get("status") == "Complete":
            break
        if time.time() > deadline:
            print("DeployStack did not finish in time", file=sys.stderr)
            return False
        time.sleep(5)
    for log in u.get("logs", []):
        text = ((log.get("stdout") or "") + (log.get("stderr") or "")).strip()
        if text and not log.get("success"):
            print(f"[{log.get('stage')}]\n" + "\n".join(redact(text).splitlines()[-20:]))
    return bool(u.get("success"))


def containers(project: str) -> list[dict]:
    out = subprocess.run(
        ["docker", "ps", "-a",
         "--filter", f"label=com.docker.compose.project={project}",
         "--filter", f"label=com.docker.compose.service={HUB_SERVICE}",
         "--format", "{{json .}}"],
        capture_output=True, text=True, check=True,
    ).stdout
    rows = [json.loads(line) for line in out.splitlines() if line.strip()]
    for row in rows:
        row["Health"] = subprocess.run(
            ["docker", "inspect", "-f",
             "{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}",
             row["ID"]],
            capture_output=True, text=True, check=True,
        ).stdout.strip()
    return rows


def wait_healthy(project: str, revision: str) -> bool:
    deadline = time.time() + HEALTH_TIMEOUT_S
    while True:
        rows = containers(project)
        if hub_ready(rows, revision):
            for r in rows:
                print(f"{r['Names']} {r['Image']} {r['Health']}")
            return True
        if time.time() > deadline:
            for r in rows:
                print(f"{r['Names']} {r['Image']} {r['Health']}", file=sys.stderr)
            print("hub container did not become healthy on the new revision", file=sys.stderr)
            return False
        time.sleep(5)


def set_revision(stack: str, revision: str) -> None:
    current = kapi("read", "GetStack", {"stack": stack})
    env = current["config"]["environment"]
    try:
        updated = rewrite_revision(env, revision)
    except ValueError as exc:
        raise SystemExit(f"stack {stack}: {exc}") from None
    kapi("write", "UpdateStack", {"id": current["_id"]["$oid"], "config": {"environment": updated}})
    print(f"HUB_BUILD_REVISION={revision}")


def main() -> int:
    if len(sys.argv) != 3 or not re.fullmatch(r"[0-9a-f]{7,40}", sys.argv[2]):
        print(__doc__, file=sys.stderr)
        return 2
    for name in ("KOMODO_API_KEY", "KOMODO_API_SECRET"):
        if not os.environ.get(name):
            print(f"missing {name}", file=sys.stderr)
            return 2
    stack, revision = sys.argv[1:]
    set_revision(stack, revision)
    if not wait_update(kapi("execute", "DeployStack", {"stack": stack, "services": []})):
        print("DeployStack failed", file=sys.stderr)
        return 1
    return 0 if wait_healthy(stack, revision) else 1


if __name__ == "__main__":
    sys.exit(main())
