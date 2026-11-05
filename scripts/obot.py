#!/usr/bin/env python3
"""Manage Obot source, CLI artifacts, and isolated local test instances."""
import argparse
from contextlib import ExitStack
import hashlib
import http.cookiejar
import io
import json
import os
from pathlib import Path
import platform
import re
import shlex
import shutil
import socket
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
import zipfile

import catalog_fixture

ROOT = Path(__file__).resolve().parents[1]
CACHE = ROOT / ".obot"
REPO = "https://github.com/obot-platform/obot.git"
IMAGE = "ghcr.io/obot-platform/obot"
PROJECT = hashlib.sha256(str(ROOT).encode()).hexdigest()[:10]
EMAIL = "test_obot_admin@example.com"
PASSWORD = "TestObotAdmin123!"
ROLES = {"basic": 4, "owner": 8, "admin": 16, "auditor": 32,
         "power-user-plus": 64, "power-user": 128}
LABEL = "ai.obot.terraform-test-project"


class SetupError(Exception):
    pass


class HTTPFailure(SetupError):
    def __init__(self, method, path, status):
        self.status = status
        super().__init__(f"{method} {path} returned HTTP {status}")


def note(message):
    print(message, file=sys.stderr, flush=True)


def run(args, *, cwd=None, env=None, capture=False):
    result = subprocess.run([str(a) for a in args], cwd=cwd, env=env,
                            text=True, stdout=subprocess.PIPE if capture else sys.stderr,
                            stderr=subprocess.PIPE if capture else None)
    if result.returncode:
        # Commands can carry passwords or API keys. Do not reproduce arguments.
        detail = (result.stderr or "").strip() if capture else ""
        raise SetupError(f"{args[0]} failed (exit {result.returncode})" +
                         (f": {detail}" if detail else ""))
    return result.stdout.strip() if capture else ""


def valid_ref(value):
    if value != "main" and not re.fullmatch(r"v\d+\.\d+\.\d+(?:-[A-Za-z0-9.-]+)?", value):
        raise argparse.ArgumentTypeError("ref must be main or a release tag such as v0.26.3")
    return value


def valid_name(value):
    if not re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,39}", value):
        raise argparse.ArgumentTypeError("instance names must be 1-40 lowercase letters, digits, _ or -")
    return value


# How far back main is searched for the commit a published image was built
# from. The main image is rebuilt continuously, so it is normally the tip.
REVISION_SEARCH_DEPTH = 300


def source(ref, refresh=False, directory=None, revision=None):
    """Return a source checkout and its commit.

    revision pins a managed main checkout to the commit a server image was
    built from, which may trail or lead the branch tip. An explicit directory
    is never moved; a mismatch is only reported.
    """
    directory = directory or os.environ.get("OBOT_SOURCE_DIR")
    if directory:
        path = Path(directory).expanduser().resolve()
        path = Path(run(["git", "-C", path, "rev-parse", "--show-toplevel"], capture=True))
        if revision:
            head = run(["git", "-C", path, "rev-parse", "HEAD"], capture=True)
            if not head.startswith(revision):
                note(f"Warning: {path} is at {head[:12]}, but the server image was built from {revision}")
    elif revision and ref == "main":
        path = CACHE / "sources" / ref
        path.parent.mkdir(parents=True, exist_ok=True)
        if not path.exists():
            run(["git", "clone", "--depth", "1", "--branch", ref, REPO, path])
        if not run(["git", "-C", path, "rev-parse", "HEAD"], capture=True).startswith(revision):
            if run(["git", "-C", path, "status", "--porcelain"], capture=True):
                raise SetupError(f"Source checkout has local changes: {path}")
            run(["git", "-C", path, "fetch", "--depth", str(REVISION_SEARCH_DEPTH), "origin", "refs/heads/main"])
            found = subprocess.run(["git", "-C", str(path), "rev-parse", "--verify", "--quiet",
                                    f"{revision}^{{commit}}"], text=True, capture_output=True)
            if found.returncode:
                raise SetupError(f"Image revision {revision} is not among the last "
                                 f"{REVISION_SEARCH_DEPTH} main commits")
            run(["git", "-C", path, "checkout", "--detach", found.stdout.strip()])
    else:
        path = CACHE / "sources" / ref
        path.parent.mkdir(parents=True, exist_ok=True)
        if not path.exists():
            run(["git", "clone", "--depth", "1", "--branch", ref, REPO, path])
        elif refresh:
            if run(["git", "-C", path, "status", "--porcelain"], capture=True):
                raise SetupError(f"Source checkout has local changes: {path}")
            run(["git", "-C", path, "fetch", "--depth", "1", "origin",
                 "refs/heads/main" if ref == "main" else f"refs/tags/{ref}"])
            run(["git", "-C", path, "checkout", "--detach", "FETCH_HEAD"])
    commit = run(["git", "-C", path, "rev-parse", "HEAD"], capture=True)
    return path, commit


def download(url):
    request = urllib.request.Request(url, headers={"User-Agent": "terraform-provider-obot-testenv"})
    with urllib.request.urlopen(request, timeout=120) as response:
        return response.read()


def release_binary(archive, name, data):
    """Extract only the requested regular file; never unpack archive paths."""
    if name.endswith(".zip"):
        with zipfile.ZipFile(io.BytesIO(archive)) as bundle:
            matches = [f for f in bundle.infolist() if f.filename == data and not f.is_dir()]
            if len(matches) == 1:
                return bundle.read(matches[0])
    else:
        with tarfile.open(fileobj=io.BytesIO(archive), mode="r:gz") as bundle:
            matches = [f for f in bundle.getmembers() if f.name == data and f.isfile()]
            if len(matches) == 1:
                return bundle.extractfile(matches[0]).read()
    raise SetupError(f"Release archive must contain one regular {data} file")


def cli(ref, refresh=False, build=False, directory=None, revision=None):
    directory = directory or os.environ.get("OBOT_SOURCE_DIR")
    system = platform.system().lower()
    arch = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64",
            "amd64": "amd64"}.get(platform.machine().lower())
    if system not in ("linux", "darwin", "windows") or not arch:
        raise SetupError(f"Unsupported CLI platform: {system}/{platform.machine()}")
    filename = "obot.exe" if system == "windows" else "obot"
    if ref == "main" or build or directory:
        path, commit = source(ref, refresh, directory, revision)
        dirty = bool(run(["git", "-C", path, "status", "--porcelain"], capture=True))
        # Never reuse a cached binary for an edited developer checkout.
        key = f"{commit}-dirty-{uuid.uuid4().hex[:8]}" if dirty else commit
        binary = CACHE / "cli" / key / f"{system}-{arch}" / filename
        if refresh or not binary.exists():
            binary.parent.mkdir(parents=True, exist_ok=True)
            run(["go", "build", "-trimpath", "-tags=kustomize_disable_go_plugin_support",
                 "-ldflags", f"-s -w -X github.com/obot-platform/obot/pkg/version.Tag={ref}-{commit[:12]}",
                 "-o", binary, "."], cwd=path)
    else:
        binary = CACHE / "cli" / ref / f"{system}-{arch}" / filename
        if refresh or not binary.exists():
            extension = "zip" if system == "windows" else "tar.gz"
            asset = f"obot_{ref}_{system}_{arch}.{extension}"
            base = f"https://github.com/obot-platform/obot/releases/download/{ref}"
            manifest = download(f"{base}/obot_{ref[1:]}_checksums.txt").decode()
            expected = [line.split()[0] for line in manifest.splitlines()
                        if len(line.split()) == 2 and line.split()[1].lstrip("*") == asset]
            if len(expected) != 1:
                raise SetupError(f"No unique checksum for {asset}")
            archive = download(f"{base}/{asset}")
            if hashlib.sha256(archive).hexdigest() != expected[0]:
                raise SetupError(f"Checksum mismatch for {asset}")
            payload = release_binary(archive, asset, filename)
            binary.parent.mkdir(parents=True, exist_ok=True)
            with tempfile.NamedTemporaryFile(dir=binary.parent, delete=False) as handle:
                handle.write(payload)
                temporary = Path(handle.name)
            temporary.chmod(0o755)
            temporary.replace(binary)
    return binary.resolve()


class API:
    def __init__(self, url, token=None):
        self.url = url.rstrip("/")
        self.token = token
        # Test instances are loopback-only; bypass external HTTP proxies.
        self.opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({}),
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def request(self, path, body=None, method=None, form=False):
        headers = {"Origin": self.url}
        if self.token:
            headers["Authorization"] = "Bearer " + self.token
        data = None
        if body is not None:
            data = urllib.parse.urlencode(body).encode() if form else json.dumps(body).encode()
            headers["Content-Type"] = ("application/x-www-form-urlencoded" if form
                                       else "application/json")
        request = urllib.request.Request(self.url + path, data=data, headers=headers, method=method)
        try:
            with self.opener.open(request, timeout=10) as response:
                payload = response.read()
                if response.headers.get_content_type() == "application/json":
                    return json.loads(payload)
                return payload.decode()
        except urllib.error.HTTPError as error:
            # Response bodies can contain secrets. Expose operation and status only.
            error.close()
            raise HTTPFailure(request.get_method(), path, error.code) from None

    def login(self, email, password):
        self.request("/oauth2/start", {"email": email, "password": password, "rd": "/"},
                     form=True)
        me = self.request("/api/me")
        if me.get("email") != email or me.get("requirePasswordChange"):
            raise SetupError(f"Local login failed for {email}")
        return me


def password():
    return os.environ.get("OBOT_TEST_PASSWORD", PASSWORD)


def state_path(name):
    return CACHE / "instances" / name / "instance.json"


def load(name):
    try:
        return json.loads(state_path(name).read_text())
    except FileNotFoundError:
        raise SetupError(f"No instance named {name}; run up first") from None


def save(state):
    path = state_path(state["name"])
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(state, indent=2) + "\n")
    temporary.replace(path)


def inspect(kind, name):
    result = subprocess.run(["docker", kind, "inspect", name], text=True,
                            capture_output=True)
    if result.returncode:
        if "no such" in result.stderr.lower():
            return None
        raise SetupError(f"Cannot inspect Docker {kind}: {result.stderr.strip()}")
    return json.loads(result.stdout)[0]


def owned(kind, name):
    data = inspect(kind, name)
    if data is None:
        return None
    labels = data.get("Config", {}).get("Labels", {}) if kind == "container" else data.get("Labels", {})
    if (labels or {}).get(LABEL) != PROJECT:
        raise SetupError(f"Refusing to manage {name}: Docker ownership label differs")
    return data


def free_port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


# Obot's run.sh checks readiness with pg_isready over the Unix socket. On first
# boot that can pass against the socket-only temporary server the Postgres
# entrypoint runs for initialization, so Obot then dials TCP 5432 before the
# real server listens and exits. This is the log line it leaves.
POSTGRES_STARTUP_RACE = "dial tcp 127.0.0.1:5432: connect: connection refused"


def postgres_startup_race(container):
    result = subprocess.run(["docker", "logs", "--tail", "50", container], text=True, capture_output=True)
    return POSTGRES_STARTUP_RACE in result.stdout + result.stderr


def wait_for(state, timeout):
    api = API(state["url"])
    deadline = time.monotonic() + timeout
    last = "not reachable"
    restarted = False
    while time.monotonic() < deadline:
        container = owned("container", state["container"])
        if not container or not container["State"]["Running"]:
            # The data volume is initialized by now, so a restart skips the
            # temporary server and cannot hit the race again. Retry only once,
            # and only for that failure, so other startup errors still fail.
            if container and not restarted and postgres_startup_race(state["container"]):
                note(f"Instance {state['name']} hit Obot's embedded PostgreSQL startup race; restarting once")
                run(["docker", "start", state["container"]])
                restarted = True
                continue
            raise SetupError(f"Instance {state['name']} stopped during startup; inspect logs")
        try:
            if api.request("/api/healthz") == "ok":
                return
        except (SetupError, urllib.error.URLError, TimeoutError, ConnectionError) as error:
            last = str(error)
        time.sleep(1)
    raise SetupError(f"Instance {state['name']} not healthy after {timeout}s: {last}")


def image_revision(version):
    """Extract the source commit from `obot version`, e.g. v0.27.0+a6070110."""
    match = re.search(r"\+([0-9a-f]{7,40})\b", version or "")
    return match.group(1) if match else None


def prepare(ref, image=None):
    """Pull the server image once and identify exactly what it contains.

    Every instance of a run starts from the returned image ID, so a tag that
    moves mid-run cannot give instances different servers.
    """
    custom_image = image is not None
    image = image or f"{IMAGE}:{ref}"
    if not custom_image or inspect("image", image) is None:
        run(["docker", "pull", image])
    details = json.loads(run(["docker", "image", "inspect", image], capture=True))[0]
    result = subprocess.run(["docker", "run", "--rm", "--network", "none", "--entrypoint", "obot",
                             details["Id"], "version"], text=True, capture_output=True, timeout=120)
    version = next((line.split(":", 1)[1].strip() for line in result.stdout.splitlines()
                    if line.startswith("Version:")), "")
    prepared = {"ref": ref, "image": image, "image_id": details["Id"],
                "image_digests": details.get("RepoDigests", []),
                "image_created": details.get("Created", ""), "server_version": version,
                "server_revision": image_revision(version)}
    # Main's image and source publish independently; build the CLI from the
    # commit the image reports so a skew between them cannot fail the run.
    prepared["cli_revision"] = prepared["server_revision"] if ref == "main" else None
    note(f"{ref}: image {details['Id'][:19]} reports {version or 'no version'}")
    return prepared


def docker_socket_path(docker_socket=None):
    default_socket = "/var/run/docker.sock"
    if Path("/etc/discobox/sandbox.json").is_file() and os.environ.get("GITHUB_ACTIONS") != "true":
        default_socket = "/run/docker.sock"
    return docker_socket or os.environ.get("OBOT_TEST_DOCKER_SOCKET", default_socket)


def run_container(state, setup_token, docker_socket, catalog_test=None):
    """Start the instance's container on its volume from state["image_id"]."""
    environment = os.environ.copy()
    environment["OBOT_SERVER_LOCAL_AUTH_INITIAL_OWNER_SETUP_TOKEN"] = setup_token
    port = urllib.parse.urlsplit(state["url"]).port
    command = ["docker", "run", "-d", "--name", state["container"],
               "--label", f"{LABEL}={PROJECT}",
               "-p", f"127.0.0.1:{port}:8080",
               "-v", f"{state['volume']}:/data",
               "-e", "OBOT_SERVER_ENABLE_AUTHENTICATION=true",
               "-e", f"OBOT_SERVER_HOSTNAME={state['url']}",
               "-e", f"OBOT_SERVER_LOCAL_AUTH_INITIAL_OWNER_EMAIL={state['email']}",
               "-e", "OBOT_SERVER_LOCAL_AUTH_INITIAL_OWNER_SETUP_TOKEN",
               "-e", "OBOT_SERVER_DEFAULT_MCPCATALOG_PATH=",
               "-e", "OBOT_SERVER_DEFAULT_SYSTEM_MCPCATALOG_PATH=",
               "-e", "OBOT_SERVER_DISABLE_UPDATE_CHECK=true"]
    if docker_socket:
        command.extend(["-v", f"{docker_socket}:/var/run/docker.sock"])
    if catalog_test:
        command.extend([
            "--add-host", "host.docker.internal:host-gateway",
            "-v", f"{catalog_test['ca_bundle']}:/etc/obot-test-catalog-ca.pem:ro",
            "-e", "SSL_CERT_FILE=/etc/obot-test-catalog-ca.pem",
            "-e", "OBOT_SERVER_DISALLOW_PRIVATE_IPMCP=false",
            "-e", f"OBOT_SERVER_DEFAULT_MCPCATALOG_PATH={catalog_test['initial']}",
            "-e", "NO_PROXY=localhost,127.0.0.1,::1,host.docker.internal",
            "-e", "no_proxy=localhost,127.0.0.1,::1,host.docker.internal",
        ])
    command.append(state["image_id"])
    run(command, env=environment)


def wait_for_catalog(state, timeout):
    # Controllers create the default catalog independently of the HTTP listener.
    api, _ = session(state)
    deadline = time.monotonic() + timeout
    while True:
        try:
            api.request("/api/mcp-catalogs/default")
            return
        except SetupError:
            if time.monotonic() >= deadline:
                raise
            time.sleep(1)


def record_provenance(state, event="start"):
    """Append what a run actually tested to OBOT_TEST_PROVENANCE, if set."""
    path = os.environ.get("OBOT_TEST_PROVENANCE")
    if not path:
        return
    keys = ("name", "ref", "image", "image_id", "image_digests", "image_created",
            "server_version", "server_revision", "cli_revision", "upgraded_from", "postgres_migration")
    record = {"event": event, **{key: state.get(key) for key in keys}}
    with open(path, "a") as handle:
        handle.write(json.dumps(record) + "\n")


def start(name, ref, port=0, timeout=180, image=None, email=EMAIL, docker_socket=None, catalog_test=None,
          prepared=None):
    docker_socket = docker_socket_path(docker_socket)
    if not 0 <= port <= 65535 or timeout <= 0:
        raise SetupError("port must be 0-65535 and timeout must be positive")
    directory = state_path(name).parent
    directory.parent.mkdir(parents=True, exist_ok=True)
    try:
        directory.mkdir()
    except FileExistsError:
        raise SetupError(f"Instance {name} already exists; use status or down") from None
    container = f"obot-tf-{PROJECT}-{name}"
    state = {"name": name, "ref": ref, "container": container,
             "volume": container + "-data", "email": email, "status": "starting"}
    save(state)
    try:
        prepared = prepared or prepare(ref, image)
        state.update({key: value for key, value in prepared.items() if key != "ref"})
        port = port or free_port()
        state["url"] = f"http://127.0.0.1:{port}"
        save(state)
        run(["docker", "volume", "create", "--label", f"{LABEL}={PROJECT}", state["volume"]])
        setup_token = uuid.uuid4().hex + uuid.uuid4().hex
        run_container(state, setup_token, docker_socket, catalog_test)
        wait_for(state, timeout)
        api = API(state["url"])
        api.request("/api/local-auth/activate", {"setupToken": setup_token})
        api.request("/api/local-auth/change-password", {"password": password()})
        identity = api.request("/api/me")
        if identity.get("email") != email:
            raise SetupError("Initial local owner identity did not match")
        wait_for_catalog(state, timeout)
        state["status"] = "ready"
        save(state)
        record_provenance(state)
        note(f"{name}: ready at {state['url']} ({ref}, local owner {email})")
        return state
    except BaseException:
        # Keep metadata and the stopped/running container for diagnostics.
        state["status"] = "failed"
        save(state)
        note(f"Startup failed; inspect with status {name} and clean up with down {name}.")
        raise


def stop(name):
    state = load(name)
    for container in (state["container"], state["container"] + "-pg"):
        if owned("container", container):
            run(["docker", "rm", "-f", container])
    if owned("volume", state["volume"]):
        run(["docker", "volume", "rm", state["volume"]])
    shutil.rmtree(state_path(name).parent)
    note(f"{name}: removed")


def postgres_major(image_id, volume=None):
    """PostgreSQL major version of an image's server, or of the cluster on a volume."""
    if volume:
        command = ["docker", "run", "--rm", "--network", "none", "-v", f"{volume}:/data",
                   "--entrypoint", "sh", image_id, "-c", 'cat "$PGDATA/PG_VERSION" 2>/dev/null || true']
    else:
        command = ["docker", "run", "--rm", "--network", "none", "--entrypoint", "postgres",
                   image_id, "--version"]
    output = run(command, capture=True)
    match = re.search(r"(\d+)", output.split("PostgreSQL)")[-1])
    return match.group(1) if match else None


def maintenance_postgres(state, image_id, environment, timeout):
    """Run only PostgreSQL from an image on the instance volume, without a network."""
    name = state["container"] + "-pg"
    run(["docker", "run", "-d", "--name", name, "--label", f"{LABEL}={PROJECT}",
         "--network", "none", "-v", f"{state['volume']}:/data",
         "-e", "POSTGRES_USER", "-e", "POSTGRES_PASSWORD", "-e", "PGDATA",
         "-e", "POSTGRES_DB=postgres", "--entrypoint", "/usr/bin/docker-entrypoint.sh",
         image_id, "postgres", "-c", "listen_addresses="], env=environment)
    deadline = time.monotonic() + timeout
    # PID 1 distinguishes the final server from the entrypoint's temporary one.
    ready = 'test "$(head -n 1 "$PGDATA/postmaster.pid" 2>/dev/null)" = 1 && pg_isready -q -U "$POSTGRES_USER" -d postgres'
    while subprocess.run(["docker", "exec", name, "sh", "-c", ready], capture_output=True).returncode:
        if time.monotonic() >= deadline:
            raise SetupError(f"Maintenance PostgreSQL for {state['name']} did not become ready")
        time.sleep(1)
    return name


def migrate_embedded_postgres(state, old_image_id, new_image_id, environment, old_major, timeout):
    """Dump and restore the embedded database across a PostgreSQL major version.

    Follows Obot's documented Docker procedure (docs/operations/
    embedded-postgresql-upgrade): dump with the old image, move the old
    cluster aside, initialize with the new image, restore, and allow only the
    documented "role already exists" error.
    """
    user = environment["POSTGRES_USER"]
    with tempfile.TemporaryDirectory(prefix="obot-pg-migration-") as directory:
        dump = Path(directory) / "backup.sql"
        name = maintenance_postgres(state, old_image_id, environment, timeout)
        try:
            with open(dump, "wb") as handle:
                result = subprocess.run(["docker", "exec", name, "pg_dumpall", "-U", user,
                                         "--quote-all-identifiers"], stdout=handle, stderr=subprocess.PIPE)
            if result.returncode or dump.stat().st_size == 0:
                raise SetupError("pg_dumpall of the embedded database failed")
        finally:
            run(["docker", "rm", "-f", name])
        run(["docker", "run", "--rm", "--network", "none", "-v", f"{state['volume']}:/data",
             "-e", "PGDATA", "--entrypoint", "sh", new_image_id, "-ec",
             f'test "$(cat "$PGDATA/PG_VERSION")" = {old_major}; '
             f'test ! -e "$PGDATA-{old_major}-backup"; mv "$PGDATA" "$PGDATA-{old_major}-backup"'],
            env=environment)
        name = maintenance_postgres(state, new_image_id, environment, timeout)
        try:
            with open(dump, "rb") as handle:
                result = subprocess.run(["docker", "exec", "-i", name, "psql", "-X", "-U", user, "-d", "postgres",
                                         "-v", "ON_ERROR_STOP=off", "-v", "VERBOSITY=terse"],
                                        stdin=handle, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
            errors = [line for line in result.stderr.decode(errors="replace").splitlines()
                      if re.search(r"(ERROR|FATAL|PANIC):", line)]
            if result.returncode or errors != [f'ERROR:  role "{user}" already exists']:
                raise SetupError(f"Restoring the embedded database reported {len(errors)} unexpected errors")
            run(["docker", "exec", name, "vacuumdb", "-U", user, "--all", "--analyze", "--quiet"])
        finally:
            run(["docker", "rm", "-f", name])


def upgrade(state, prepared, timeout=180, docker_socket=None, catalog_test=None):
    """Replace an instance's server with another image, keeping its data volume.

    This is what an operator does to upgrade: the database, users, API keys,
    and every object Terraform created survive and must still be readable.
    """
    if not owned("volume", state["volume"]):
        raise SetupError(f"Instance {state['name']} has no data volume to upgrade")
    container = owned("container", state["container"])
    if not container:
        raise SetupError(f"Instance {state['name']} has no container to upgrade")
    database = {key: value for key, _, value in (entry.partition("=") for entry in container["Config"]["Env"])
                if key in ("POSTGRES_USER", "POSTGRES_PASSWORD", "PGDATA")}
    run(["docker", "stop", "--time", "60", state["container"]])
    run(["docker", "rm", state["container"]])
    old_major = postgres_major(prepared["image_id"], state["volume"])
    new_major = postgres_major(prepared["image_id"])
    if old_major and new_major and old_major != new_major:
        if set(database) != {"POSTGRES_USER", "POSTGRES_PASSWORD", "PGDATA"}:
            raise SetupError("Cannot migrate the embedded database: its settings are not in the container")
        note(f"{state['name']}: migrating embedded PostgreSQL {old_major} to {new_major} "
             "with Obot's documented dump and restore")
        environment = os.environ.copy()
        environment.update(database)
        migrate_embedded_postgres(state, state["image_id"], prepared["image_id"], environment, old_major, timeout)
        state["postgres_migration"] = f"{old_major}->{new_major}"
    previous = {key: state.get(key) for key in ("ref", "server_version")}
    state.update(prepared)
    state["status"] = "upgrading"
    state["upgraded_from"] = previous
    save(state)
    # The owner already exists, so this setup token is never accepted.
    run_container(state, uuid.uuid4().hex + uuid.uuid4().hex, docker_socket_path(docker_socket), catalog_test)
    wait_for(state, timeout)
    wait_for_catalog(state, timeout)
    state["status"] = "ready"
    save(state)
    record_provenance(state, "upgrade")
    note(f"{state['name']}: upgraded from {previous['server_version']} to {state['server_version']}")
    return state


def logs(name):
    state = load(name)
    data = owned("container", state["container"])
    if not data:
        return "No container was created.\n"
    result = subprocess.run(["docker", "logs", "--tail", "200", state["container"]],
                            text=True, capture_output=True)
    text = result.stdout + result.stderr
    # Defaults baked into the published image, such as its embedded database
    # password, are public; redacting them would erase common words like the
    # product name from every line. Redact everything set at run time.
    image = inspect("image", data.get("Image", "")) or {}
    public = set((image.get("Config") or {}).get("Env") or [])
    for entry in data.get("Config", {}).get("Env", []):
        key, _, value = entry.partition("=")
        if entry in public:
            continue
        if value and any(word in key for word in ("TOKEN", "PASSWORD", "SECRET", "API_KEY")):
            text = text.replace(value, "[REDACTED]")
    text = re.sub(r"ok1-[A-Za-z0-9_-]+", "[REDACTED_API_KEY]", text)
    path = CACHE / "logs" / f"{name}.log"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    note(f"Redacted logs: {path}")
    return text


def session(state, email=None):
    api = API(state["url"])
    identity = api.login(email or state["email"], password())
    return api, identity


def token(state, email=None):
    api, identity = session(state, email)
    # A seeded API key lets the upstream CLI login validate and print a token
    # without opening a browser or depending on a desktop OS keyring in CI.
    key = api.request("/api/api-keys", {
        "name": "Terraform tests " + uuid.uuid4().hex[:8], "canAccessAPI": True})
    binary = cli(state["ref"], revision=state.get("cli_revision"))
    environment = os.environ.copy()
    environment["OBOT_BASE_URL"] = state["url"] + "/api"
    environment["OBOT_TOKEN"] = key["key"]
    result = subprocess.run([str(binary), "login", "--scope", "api", "--print-token"],
                            env=environment, text=True, capture_output=True, timeout=60)
    if result.returncode:
        raise SetupError("obot login --scope api --print-token failed")
    value = result.stdout.strip()
    if value != key["key"]:
        raise SetupError("obot login returned an unexpected token")
    verified = API(state["url"], value).request("/api/me")
    if verified.get("email") != identity["email"]:
        raise SetupError("API token resolved to a different user")
    return value


def add_user(state, email, role):
    api, _ = session(state)
    api.request("/api/local-auth/users",
                {"email": email, "password": password(), "requirePasswordChange": False})
    user_api = API(state["url"])
    identity = user_api.login(email, password())
    api.request("/api/users/" + identity["id"], {"role": ROLES[role]}, method="PATCH")
    identity = user_api.request("/api/me")
    if identity.get("role") != ROLES[role]:
        raise SetupError(f"Role for {email} did not become {role}")
    note(f"{state['name']}: created {email} with role {role}")


def environment_for(states, email=None):
    records = [{"name": state["name"], "base_url": state["url"] + "/api",
                "email": email or state["email"], "token": token(state, email)} for state in states]
    environment = os.environ.copy()
    environment.update({"OBOT_BASE_URL": records[0]["base_url"],
                        "OBOT_TOKEN": records[0]["token"],
                        "OBOT_TEST_EMAIL": records[0]["email"],
                        "OBOT_TEST_INSTANCES": json.dumps(records),
                        "TF_ACC": "1"})
    # CLI commands run with this environment use the matching cached CLI.
    binary = cli(states[0]["ref"], revision=states[0].get("cli_revision"))
    environment["PATH"] = str(binary.parent) + os.pathsep + environment.get("PATH", "")
    return environment


def terraform(name, action, arguments):
    state = load(name)
    config = ROOT / "dev.tfrc"
    if not config.is_file():
        raise SetupError("Run make dev first to build and configure the local provider")
    environment = environment_for([state])
    directory = CACHE / "terraform" / name
    directory.mkdir(parents=True, exist_ok=True)
    # Run each instance in its own root directory so Terraform's default local
    # state path is isolated. Link configuration so edits remain visible.
    example = ROOT / "examples" / "dev"
    suffixes = (".tf", ".tf.json", ".tfvars", ".tfvars.json")
    files = {path.name: path for path in example.iterdir()
             if path.is_file() and path.name.endswith(suffixes)}
    for path in directory.iterdir():
        if path.is_symlink() and path.name.endswith(suffixes) and path.name not in files:
            path.unlink()
    for filename, source_path in files.items():
        destination = directory / filename
        if destination.is_symlink():
            destination.unlink()
        destination.symlink_to(source_path)
    environment["TF_CLI_CONFIG_FILE"] = str(config)
    environment["TF_DATA_DIR"] = str(directory / ".terraform")
    command = ["terraform", f"-chdir={directory}"]
    command.extend(["plan", "-destroy"] if action == "plan-destroy" else [action])
    command.extend(arguments)
    note(f"{name}: terraform {action}; state in {directory}")
    # Inherit the terminal so apply/destroy retain Terraform's approval prompt.
    return subprocess.run(command, env=environment).returncode


def smoke(states):
    records = json.loads(environment_for(states)["OBOT_TEST_INSTANCES"])
    for state, record in zip(states, records):
        me = API(state["url"], record["token"]).request("/api/me")
        if me["email"] != state["email"]:
            raise SetupError("Owner identity differs")
        anonymous = {}
        try:
            anonymous = API(state["url"]).request("/api/me")
        except HTTPFailure as error:
            if error.status not in (401, 403):
                raise
        if anonymous.get("email") == state["email"]:
            raise SetupError("Instance admitted an anonymous request as owner")
        API(state["url"], record["token"]).request("/api/local-auth/users")
        # A second local account exercises password login and role separation.
        add_user(state, "test_obot_user@example.com", "basic")
        basic_key = token(state, "test_obot_user@example.com")
        basic = API(state["url"], basic_key).request("/api/me")
        if basic["role"] != ROLES["basic"]:
            raise SetupError("Basic test user has an unexpected role")
        try:
            API(state["url"], basic_key).request("/api/local-auth/users")
        except HTTPFailure as error:
            if error.status != 403:
                raise
        else:
            raise SetupError("Basic test user could access owner/admin-only APIs")
        for other in records:
            if record["name"] == other["name"]:
                continue
            try:
                me = API(state["url"], other["token"]).request("/api/me")
            except HTTPFailure as error:
                if error.status not in (401, 403):
                    raise
                continue
            if me.get("email") == state["email"]:
                raise SetupError("An API token authenticated against a different instance")
        note(f"{state['name']}: owner login, basic-user login, and token isolation passed")
    if len(states) > 1:
        check_provider_aliases(states, records)



def build_provider(path):
    """Compile this checkout's provider and write a Terraform CLI config that uses it.

    Returns the CLI config path. Terraform then runs the real plugin binary,
    exactly as a user's installation would, instead of the in-process harness.
    """
    binary_dir = path / "bin"
    binary_dir.mkdir(parents=True, exist_ok=True)
    binary_name = "terraform-provider-obot.exe" if os.name == "nt" else "terraform-provider-obot"
    run(["go", "build", "-o", binary_dir / binary_name, "."], cwd=ROOT)
    override = path / "dev.tfrc"
    override.write_text("provider_installation {\n  dev_overrides {\n"
                        '    "registry.terraform.io/obot-platform/obot" = ' +
                        json.dumps(str(binary_dir)) + "\n  }\n  direct {}\n}\n")
    return override


def check_provider_aliases(states, records):
    """Exercise aliases with real plugin processes, outside the reattach harness."""
    with tempfile.TemporaryDirectory(prefix="obot-provider-aliases-") as directory:
        path = Path(directory)
        override = build_provider(path)
        config = """
terraform {
  required_providers {
    obot = {
      source = "obot-platform/obot"
    }
  }
}
variable "first_token" {
  type = string
  sensitive = true
}
variable "second_token" {
  type = string
  sensitive = true
}
provider "obot" {
  alias = "first"
  base_url = FIRST_URL
  token = var.first_token
}
provider "obot" {
  alias = "second"
  base_url = SECOND_URL
  token = var.second_token
}
data "obot_current_user" "first" {
  provider = obot.first
}
data "obot_current_user" "second" {
  provider = obot.second
}
output "first_email" {
  value = data.obot_current_user.first.email
}
output "second_email" {
  value = data.obot_current_user.second.email
}
"""
        config = config.replace("FIRST_URL", json.dumps(records[0]["base_url"]))
        config = config.replace("SECOND_URL", json.dumps(records[1]["base_url"]))
        (path / "main.tf").write_text(config)
        environment = os.environ.copy()
        environment.pop("TF_REATTACH_PROVIDERS", None)
        environment.update({
            "TF_CLI_CONFIG_FILE": str(override),
            "TF_DATA_DIR": str(path / "terraform-data"),
            "TF_VAR_first_token": records[0]["token"],
            "TF_VAR_second_token": records[1]["token"],
        })
        plan = path / "plan"
        run(["terraform", f"-chdir={path}", "plan", "-input=false", "-out", plan],
            env=environment, capture=True)
        result = json.loads(run(["terraform", f"-chdir={path}", "show", "-json", plan],
                                env=environment, capture=True))
        outputs = result["planned_values"]["outputs"]
        if (outputs["first_email"]["value"] != states[0]["email"] or
                outputs["second_email"]["value"] != states[1]["email"]):
            raise SetupError("Compiled provider aliases connected to the wrong Obot identity")
        note("Compiled provider aliases read the correct identities from both instances.")


def main(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    child_args = []
    if argv and argv[0] in ("exec", "testacc", "terraform") and "--" in argv:
        separator = argv.index("--")
        child_args, argv = argv[separator + 1:], argv[:separator]
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    for command in ("source", "cli"):
        p = sub.add_parser(command)
        p.add_argument("--ref", type=valid_ref, default="main")
        p.add_argument("--refresh", action="store_true")
        p.add_argument("--directory", type=Path, default=os.environ.get("OBOT_SOURCE_DIR"))
        if command == "cli":
            p.add_argument("--build", action="store_true")
    p = sub.add_parser("up")
    p.add_argument("name", type=valid_name)
    p.add_argument("--ref", type=valid_ref, default="main")
    p.add_argument("--port", type=int, default=0)
    p.add_argument("--timeout", type=int, default=180)
    p.add_argument("--email", default=EMAIL)
    p.add_argument("--image")
    p.add_argument("--docker-socket", default=None)
    p = sub.add_parser("terraform")
    p.add_argument("name", type=valid_name)
    p.add_argument("action", choices=("plan", "apply", "plan-destroy", "destroy"))
    for command in ("down", "status", "logs", "env", "token", "user-add", "exec"):
        p = sub.add_parser(command)
        p.add_argument("name", type=valid_name)
        if command in ("token", "exec"):
            p.add_argument("--email")
        if command == "user-add":
            p.add_argument("email")
            p.add_argument("--role", choices=ROLES, default="basic")
    for command in ("smoke", "testacc"):
        p = sub.add_parser(command)
        p.add_argument("--ref", type=valid_ref, default="main")
        p.add_argument("--instances", type=int, default=1)
        p.add_argument("--keep", action="store_true")
        p.add_argument("--timeout", type=int, default=180)
    args = parser.parse_args(argv)
    if args.command == "source":
        path, commit = source(args.ref, args.refresh, args.directory)
        note(f"{args.ref}: {commit}")
        print(path)
    elif args.command == "cli":
        binary = cli(args.ref, args.refresh, args.build, args.directory)
        run([binary, "version"])
        print(binary)
    elif args.command == "up":
        print(json.dumps(start(args.name, args.ref, args.port, args.timeout,
                               args.image, args.email, args.docker_socket), indent=2))
    elif args.command == "down":
        stop(args.name)
    elif args.command == "status":
        state = load(args.name)
        data = owned("container", state["container"])
        state["running"] = bool(data and data["State"]["Running"])
        print(json.dumps(state, indent=2))
    elif args.command == "logs":
        print(logs(args.name), end="")
    elif args.command == "env":
        state = load(args.name)
        binary = cli(state["ref"], revision=state.get("cli_revision"))
        print("export OBOT_BASE_URL=" + shlex.quote(state["url"] + "/api"))
        print("export PATH=" + shlex.quote(str(binary.parent)) + ':"$PATH"')
        print("unset OBOT_TOKEN")
    elif args.command == "token":
        print(token(load(args.name), args.email))
    elif args.command == "user-add":
        add_user(load(args.name), args.email, args.role)
    elif args.command == "exec":
        command = child_args
        if not command:
            parser.error("exec requires a command after --")
        result = subprocess.run(command, env=environment_for([load(args.name)], args.email))
        return result.returncode
    elif args.command == "terraform":
        return terraform(args.name, args.action, child_args)
    elif args.command in ("smoke", "testacc"):
        if args.instances < 1:
            parser.error("--instances must be positive")
        states, names = [], []
        fixtures = ExitStack()
        catalog_test = fixtures.enter_context(catalog_fixture.serve()) if args.command == "testacc" else None
        try:
            prepared = prepare(args.ref)
            cli(args.ref, revision=prepared["cli_revision"])
            for index in range(args.instances):
                name = f"test-{uuid.uuid4().hex[:8]}-{index}"
                names.append(name)
                email = EMAIL if index == 0 else f"test_obot_admin_{index + 1}@example.com"
                states.append(start(name, args.ref, timeout=args.timeout, email=email,
                                    catalog_test=catalog_test, prepared=prepared))
            if args.command == "smoke":
                smoke(states)
            else:
                command = child_args
                command = command or ["go", "test", "-count=1", "-timeout=60m", "-run=TestAcc", "./internal/provider/..."]
                environment = environment_for(states)
                environment.update({
                    "OBOT_TEST_MANAGE_DEFAULTS": "1",
                    "OBOT_TEST_CONTAINER": states[0]["container"],
                    "OBOT_TEST_CATALOG_SOURCE_INITIAL": catalog_test["initial"],
                    "OBOT_TEST_CATALOG_SOURCE_REPLACEMENT": catalog_test["replacement"],
                    "OBOT_TEST_CATALOG_SOURCE_EXTRA": catalog_test["extra"],
                    "OBOT_TEST_CATALOG_GIT_SOURCE": catalog_test["private"],
                    "OBOT_TEST_CATALOG_GIT_HOST": catalog_test["private_host"],
                    "OBOT_TEST_CATALOG_GIT_TOKEN": catalog_test["private_token"],
                })
                result = subprocess.run(command, env=environment)
                if result.returncode:
                    for name in names:
                        logs(name)
                return result.returncode
        except BaseException:
            for name in names:
                if state_path(name).exists():
                    try:
                        logs(name)
                    except (SetupError, OSError):
                        pass
            raise
        finally:
            if args.keep:
                if catalog_test:
                    note("HTTPS catalog fixtures stopped; kept instances retain their test CA and source URLs for diagnosis only.")
                note("Kept instances: " + ", ".join(names))
            else:
                for name in reversed(names):
                    if state_path(name).exists():
                        stop(name)
            fixtures.close()
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (SetupError, urllib.error.URLError, TimeoutError, ConnectionError, OSError,
            subprocess.TimeoutExpired) as error:
        note(f"Error: {error}")
        sys.exit(1)
