# Terraform Provider for Obot

> [!WARNING]
> This provider is under active development. Resources, attributes, and
> behavior are subject to change.

A Terraform provider for Obot, built with the Terraform Plugin Framework.

The provider connects with an API-scoped key and manages MCP tunnels,
individual default MCP catalog sources, remote MCP catalog entries, Git
credentials for private sources, and catalog MCP access policies. Read-only data sources expose current
identity, authentication-provider metadata, tunnel metadata, catalog
inventory, Git credential IDs, and access policies. It is published on the
Terraform Registry as [`obot-platform/obot`](https://registry.terraform.io/providers/obot-platform/obot).

See the [catalog and access-policy guide](docs/guides/catalogs-and-access-policies.md)
for adding, adopting, and removing catalog sources, looking up the
current authentication provider, and importing or removing Everything.
See the [tunnel guide](docs/guides/mcp-tunnels.md) for tunnel credentials and import.

## Use the provider

Obtain an API-scoped key for the target server with the Obot CLI:

```shell
export OBOT_BASE_URL="https://obot.example.com/api"
export OBOT_TOKEN="$(obot login --scope api --print-token)"
```

Then use the provider:

```hcl
terraform {
  required_providers {
    obot = {
      source  = "obot-platform/obot"
      version = "~> 0.1"
    }
  }
}

provider "obot" {}

data "obot_current_user" "current" {}
```

The provider requires Terraform 1.0 or later; write-only Git credential tokens
require Terraform 1.11 or later, and importing by resource identity
requires Terraform 1.12 or later. Releases are tested with Terraform 1.14
against Obot v0.26.3 and `main`. Use an Owner or Admin API key to manage
catalog sources and access policies.

The provider accepts `base_url` and sensitive `token` attributes; explicit
values take precedence over `OBOT_BASE_URL` and `OBOT_TOKEN`. Supply an app
URL or API base URL; a trailing `/api` is added when needed. Configuration
validates API-key scope and the authenticated identity before reads.
Each provider configuration owns an independent client, including aliases
pointing at different Obot instances.

To test unreleased provider changes, use the development overrides described
under [Build and local Terraform development](#build-and-local-terraform-development).

## Development requirements

- Go >= 1.25.8 for the provider, as specified in `go.mod`
- Go >= 1.27 to build the current Obot CLI from source
- Terraform >= 1.13
- Python >= 3.10 for local test tooling
- Docker running locally for Obot test instances
- OpenSSL >= 1.1.1 for the HTTPS catalog test fixtures
- GNU Make and golangci-lint v2.14.0

Provider source: `obot-platform/obot`. Go module:
`github.com/obot-platform/terraform-provider-obot`.

## Obtain Obot source and CLI

Two modes are supported:

| Ref | Server | CLI |
| --- | --- | --- |
| `main` (default) | Published `ghcr.io/obot-platform/obot:main` image | Built from the main source checkout |
| Version tag, such as `v0.26.3` | Published image for that tag | GitHub release binary verified against its published checksum |

```shell
make obot-source
make obot-cli
make obot-source OBOT_REF=v0.26.3
make obot-cli OBOT_REF=v0.26.3
```

Source and CLI caches live under ignored `.obot/`. Main is intentionally a
moving development target. Cached source advances with an explicit refresh;
each CLI build is cached by the resolved source commit. The image is pulled
when an instance starts, and its actual ID and digest are recorded. The main
image and source can be published at different times.

```shell
python3 scripts/obot.py cli --ref main --refresh
python3 scripts/obot.py cli --ref v0.26.3 --build
python3 scripts/obot.py source --ref main --directory /path/to/obot
python3 scripts/obot.py cli --ref main --directory /path/to/obot
```

An existing checkout is used without changing its files or Git refs. You can
also set `OBOT_SOURCE_DIR`. Managed source refresh refuses local changes.
CLI/source commands print the resulting path to stdout and progress to stderr.
Using a release CLI does not require checking out or building Obot.

## Run one or more test instances

```shell
make obot-up OBOT_INSTANCE=dev
make obot-up OBOT_INSTANCE=release OBOT_REF=v0.26.3
python3 scripts/obot.py status dev
python3 scripts/obot.py status release
```

Each named instance gets its own loopback port, data volume, database, local
auth sessions, and API keys. The default initial account is:

- Email: `test_obot_admin@example.com`
- Password: `TestObotAdmin123!`
- Role: Owner, including administrative permissions

These are public credentials for disposable local test instances.
Set `OBOT_TEST_PASSWORD` consistently during creation and subsequent commands
to use another password. Use `up --email` to choose another initial owner.
The automatic multiple-instance test runner uses
`test_obot_admin_2@example.com`, `test_obot_admin_3@example.com`, etc. for
additional owners so tests can detect connections to the wrong instance.

Startup activates the local owner, sets the password, and waits for the
default catalog. No external identity provider or model-provider key is needed.

```shell
eval "$(python3 scripts/obot.py env dev)"
OBOT_TOKEN="$(python3 scripts/obot.py token dev)" && export OBOT_TOKEN
```

This works in a sandbox or terminal without a desktop keyring. The helper
signs in with the known local-auth credentials, creates an API-scoped key,
and invokes `obot login --scope api --print-token` with that key in its
environment. The assignment captures the token without printing it.

On a desktop with a working OS keyring, you can instead use a fresh
interactive login:

```shell
OBOT_TOKEN="$(obot login --scope api --print-token)" && export OBOT_TOKEN
```

Sign in using the credentials above and complete its device-code prompt.
The CLI stores the key in the OS keyring. An error mentioning
`org.freedesktop.secrets` means the Linux Secret Service is unavailable;
use the test helper command above for the named local instance.

For headless tests, run a child command with credentials prepared automatically:

```shell
make test-acceptance
```

The helper signs in with local auth, creates an API-scoped seed key, and runs
`obot login --scope api --print-token` with that key. It validates the
resulting identity and passes the token only through the child environment.
This works without a browser or desktop keyring in CI.

Add users as tests need them:

```shell
python3 scripts/obot.py user-add dev test_obot_user@example.com --role basic
python3 scripts/obot.py exec dev --email test_obot_user@example.com -- make test-acceptance-env
```

Additional users use the same test password. Available roles are `basic`,
`admin`, `owner`, `auditor`, `power-user`, and `power-user-plus`.

Use `up --port 8080` for a fixed port, or `up --image IMAGE` for a custom
image. Obot's Docker backend requires its daemon socket during startup.
Inside Discobox, the tooling mounts `/run/docker.sock:/var/run/docker.sock`.
Other environments, including GitHub Actions, use
`/var/run/docker.sock:/var/run/docker.sock`. Set `OBOT_TEST_DOCKER_SOCKET`
or use `up --docker-socket` to override the socket path on the Docker host.

Inspect failures and remove instances by name:

```shell
python3 scripts/obot.py logs dev
make obot-down OBOT_INSTANCE=dev
make obot-down OBOT_INSTANCE=release
```

Failed manual startup preserves its container and metadata for diagnosis.
The logs command saves sanitized output to `.obot/logs/`. Teardown checks
project ownership labels and removes only the named instance's container and
volume.

## Run tests

Run `make help` to see the commands. The three usual choices are:

| Command | Obot instance | Authentication and cleanup |
| --- | --- | --- |
| `make test` | None | Runs Go and Python unit tests; always disables acceptance tests |
| `make test-acceptance` | Existing `dev` instance | Logs in automatically; leaves the instance running |
| `make test-acceptance-fresh` | Creates fresh instances | Logs in automatically; removes the instances afterward |

```shell
make test
make test-acceptance
make test-acceptance OBOT_INSTANCE=release
make test-acceptance-fresh OBOT_REF=main OBOT_INSTANCES=2
make test-acceptance-fresh OBOT_REF=v0.26.3 OBOT_INSTANCES=2
```

Fresh acceptance runs include tests that add, adopt, and remove default catalog
sources while leaving other sources and API-created entries in place, sync a
private Git source through a Git credential, import the seeded Everything policy,
and delete that policy while preserving another rule. The HTTPS fixture
server and temporary CA live only for the run. `openssl` is required for
these tests. `--keep` retains containers for diagnosis but stops the fixture
server. Existing-instance runs skip tests that change seeded defaults unless
`OBOT_TEST_MANAGE_DEFAULTS=1` is explicitly supplied; catalog tests also need
`OBOT_TEST_CATALOG_SOURCE_INITIAL`, `OBOT_TEST_CATALOG_SOURCE_REPLACEMENT`, and
`OBOT_TEST_CATALOG_SOURCE_EXTRA` pointing to deterministic, trusted fixture sources,
and the private Git source test needs `OBOT_TEST_CATALOG_GIT_SOURCE`,
`OBOT_TEST_CATALOG_GIT_HOST`, and `OBOT_TEST_CATALOG_GIT_TOKEN`. Use fresh instances for
the complete suite.

Choose individual acceptance tests with
`TESTARGS='-run=TestAccCurrentUser_basic'` on any acceptance target.
An existing instance must first have been created with `make obot-up` or
`scripts/obot.py up`; its saved metadata supplies the URL and owner email.
For an external instance, set `OBOT_BASE_URL`, `OBOT_TOKEN`, and
`OBOT_TEST_EMAIL`, then run `make test-acceptance-env`.

`make test-smoke OBOT_INSTANCES=2` creates fresh instances and checks Owner
and Basic-user login, denied administrative access for Basic users, and
isolation between instances. With multiple instances it also runs a Terraform plan with the
compiled provider and aliases, checking each server's distinct identity.

The fresh acceptance runner prepares the matching CLI, starts fresh
instances, runs Terraform acceptance tests, and removes the instances.
Use `python3 scripts/obot.py testacc --ref main --instances 2 --keep` to
preserve them for diagnosis. Both runners save sanitized logs on failure.
A custom test command can follow `--`.

Tests receive `OBOT_BASE_URL`, `OBOT_TOKEN`, `OBOT_TEST_EMAIL`, `TF_ACC=1`,
and `OBOT_TEST_INSTANCES` (a JSON array of named connections). Tokens are not
written to instance metadata.

Manual test instances start with empty MCP catalog sources. Fresh acceptance
instances use deterministic catalogs served locally over verified HTTPS: three
public YAML files and a private Git repository requiring a token generated for
the run. They trust a temporary test CA and allow the Docker host's private
address; these fixtures need no public catalog or external GitHub credentials.

The older `testacc`, `testacc-local`, and `obot-smoke` targets remain aliases
for `test-acceptance-env`, `test-acceptance-fresh`, and `test-smoke`.

The in-process HashiCorp acceptance harness does not isolate provider aliases;
the multi-instance acceptance test uses separate factory names. The compiled
provider's actual alias behavior is checked separately by the smoke suite.

## Build and local Terraform development

Use the existing instance's name as the target prefix:

```shell
make dev-plan
make dev-apply
make dev-plan
make dev-plan-destroy
make dev-destroy
```

For an instance named `release`, use `make release-plan`, `make release-apply`,
and so on. Create the named instance with `make obot-up OBOT_INSTANCE=release`
first if needed. These targets rebuild the provider, configure its local
development override, and authenticate automatically without a desktop keyring.
They leave the Obot Docker instance running.

The shared configuration is `examples/dev/main.tf`, which reads and outputs
the authenticated user's email. Each instance has independent local Terraform
state and plugin data under ignored `.obot/terraform/NAME/`. The helper uses
Terraform's local backend; keep the development example on that backend.

`plan` previews changes. `apply` generates a fresh plan and asks for approval.
`plan-destroy` runs `terraform plan -destroy` to preview removal; `destroy`
asks for approval before removing Terraform-managed objects. By default, the
helpers do not save plan files. The current example only reads a data source,
so destruction removes its state and output without deleting the Obot user.
Pass extra Terraform options with `TFARGS`, for example
`make dev-plan TFARGS=-no-color`.

Lower-level build and configuration commands are also available:

```shell
make build
make lint
make dev
export TF_CLI_CONFIG_FILE="$PWD/dev.tfrc"
terraform -chdir=examples/dev providers schema -json
```

`make build` writes `bin/terraform-provider-obot`; `make install` installs
to `GOBIN`, or `$(go env GOPATH)/bin` when unset. `make dev` writes an ignored
checkout-specific CLI configuration using
[Terraform development overrides](https://developer.hashicorp.com/terraform/cli/config/config-file#development-overrides-for-provider-developers).
Re-run it after rebuilding or moving the checkout.

Skip `terraform init` for this example: it still attempts to resolve the
unpublished provider in the Registry. Terraform's development-override warning
is expected. `make dev-check` checks the schema and validates the configuration
without connecting to Obot. Prefer the named Make targets for plan/apply so
authentication and Terraform state stay associated with the same instance.

Use `unset TF_CLI_CONFIG_FILE` to return to your normal Terraform settings.

## MCP tunnels

Use `obot_mcp_tunnel` to define a tunnel and capture its one-time, sensitive
agent token. `obot_mcp_tunnel` and `obot_mcp_tunnels` data sources return
metadata and a token preview; Obot cannot recover a full token through reads.

```hcl
resource "obot_mcp_tunnel" "customer" {
  display_name           = "Customer private network"
  allowed_urls           = ["https://mcp.internal.example/mcp"]
  token_rotation_version = 0
}
```

Import an existing definition by its generated ID:

```shell
terraform import obot_mcp_tunnel.customer mt1example
```

Terraform 1.12+ can also import with an `import` block's `identity = { id = ... }`.

Match its metadata and keep `token_rotation_version = 0` for adoption. Import
preserves its ID and credential, leaves connected agents alone, and warns
that `token` is null. It never substitutes a preview. Metadata updates and
refresh do not rotate the credential. With matching configuration, the next
plan has no changes.

Increase `token_rotation_version` to explicitly rotate and capture a new token.
Obot immediately revokes the previous credential and disconnects its agents;
update their secret and restart them. If Obot detects an external rotation,
refresh clears the stale token and warns. Secret-export examples reject a
null token with a precondition. Sensitive tokens remain in Terraform state;
use a protected backend.

The [tunnel guide](docs/guides/mcp-tunnels.md) walks through creation, AWS and
Vault publication, agent startup, import, rotation, recovery, and deletion.
See the [resource reference](docs/resources/mcp_tunnel.md) for its schema and
both import forms.

To put a tunnel behind a catalog entry, use `obot_mcp_catalog_entry`, which
manages remote entries created directly in the default catalog, and grant
access with `obot_mcp_access_rule`. Obot does not accept tunnel references in
entries synchronized from a catalog source. Header values are write-only
(`value_wo`, Terraform 1.11+). See the
[resource reference](docs/resources/mcp_catalog_entry.md).

Acceptance tests cover creation, metadata changes, empty allowlists, stable
refresh, explicit rotation, live CLI connectivity and revoked-token rejection,
CLI and import-block adoption, null-token export rejection, external deletion
and drift, data-source lookup/listing, invalid inputs, dependency failures,
and destruction. Unit tests also cover API permission failures, safe error
redaction, malformed token responses, and partially successful updates.

## Skills and documentation

The project skills are versioned under `.agents/skills/`:

- `obot-source`: acquire and inspect upstream source
- `obot-cli`: obtain the CLI and authenticate
- `obot-test-instance`: manage isolated Docker instances and test users

Run `make generate` after schema or example changes and commit the generated
`docs/` files. The generator is pinned in `tools/go.mod`.

Preview the generated documentation locally with:

```shell
make docs-preview
```

Open `http://localhost:8765/`, or set `DOCS_PORT` to choose another port.
The command installs pinned preview dependencies in an isolated environment
under ignored `.obot/` and serves the existing `docs/` files. Run
`make generate` first when schema or example changes need to be regenerated.
Stop the preview with Ctrl-C.

Use this command for local previews so their appearance stays consistent.
The checked-in theme uses a white background, dark text, light-gray navigation,
restrained blue links, and standard syntax highlighting. It stays light
regardless of desktop color preferences. Keep theme changes in
`scripts/docs_preview.css`; do not recreate temporary preview servers with
other color schemes. The local preview approximates Terraform Registry;
use the [official Registry documentation preview](https://registry.terraform.io/tools/doc-preview)
for final rendering checks. Published docs remain Markdown, styled by the Registry.

## CI and releases

CI builds and lints the provider, runs Go and tooling unit tests, and checks
generated documentation. The acceptance workflow exercises both `main` and
`v0.26.3` with two Docker instances using the same commands as local development.

Pushing a `v*` tag, such as `v0.1.0`, runs GoReleaser to build, sign, and
publish a GitHub release, which the Terraform Registry then picks up.
[RELEASING.md](RELEASING.md) covers the one-time Registry setup and the
release checklist.

## License

MPL-2.0. Derived from
[HashiCorp's Terraform Plugin Framework scaffold](https://github.com/hashicorp/terraform-provider-scaffolding-framework).
