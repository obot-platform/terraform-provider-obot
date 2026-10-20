## 0.1.0 (Unreleased)

Initial release.

FEATURES:

- **New provider:** connects to Obot with an API-scoped key from `base_url` and
  `token` or `OBOT_BASE_URL` and `OBOT_TOKEN`, and validates the key's scope
  and identity. API error diagnostics include the server's explanation, with
  credentials and token-like values redacted.
- **New resource:** `obot_mcp_tunnel` manages an MCP tunnel definition. It
  captures the one-time token on create, rotates only when
  `token_rotation_version` changes, detects external rotation, and imports by
  ID or resource identity without rotating.
- `obot_mcp_tunnel` exports sensitive `command` and `docker_command`
  attributes, the ready-to-run `obot tunnel` commands the Obot UI shows. They
  are set with the token on create and rotation, and the Docker image tag
  follows the Obot server version, falling back to `main`.
- **New resource:** `obot_mcp_catalog_source` manages one default catalog
  source the way the Obot UI does, leaving other sources unchanged. It waits
  for the catalog to sync, reports only its own sync errors, and imports by
  source URL or resource identity.
- **New resource:** `obot_git_credential` manages a token Obot uses to read
  private Git sources on one host. The token can be write-only (`token_wo`,
  Terraform 1.11+) so it never reaches state, and it imports by ID or resource
  identity.
- **New resource:** `obot_mcp_access_rule` manages one MCP access policy,
  including the shipped Everything policy, and imports by rule ID or resource
  identity.
- **New resource:** `obot_mcp_catalog_entry` manages a remote MCP server entry
  created directly in the default catalog, including one reached through an
  MCP tunnel, which catalog sources cannot define. Fixed header values are
  write-only (`value_wo`, Terraform 1.11+) and are resent only when
  `value_wo_version` changes. Entries synchronized from a catalog source are
  rejected. It imports by ID or resource identity.
- **New data sources:** `obot_current_user`, `obot_current_auth_provider`,
  `obot_mcp_tunnel`, `obot_mcp_tunnels`, `obot_mcp_catalog`,
  `obot_git_credential` (by ID or unique display name), `obot_git_credentials`,
  `obot_mcp_access_rule`, and `obot_mcp_access_rules`.
- **New guides:** MCP tunnels (import, agents, AWS Secrets Manager, and Vault),
  and managing catalog sources and access policies.

NOTES:

- Tested with Terraform 1.14 against Obot v0.26.3 and `main`. Write-only
  tokens require Terraform 1.11 or later, and importing by resource identity
  requires Terraform 1.12 or later.
