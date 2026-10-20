# Examples

- `provider/provider.tf` is included in the generated provider documentation.
- `dev/main.tf` is the local development smoke configuration. Run it using
  the development overrides described in the root README.

- `resources/obot_mcp_tunnel/` shows creation and both import forms. Replace
  the example import ID with an existing tunnel's generated ID before importing.
- `data-sources/obot_mcp_tunnel/` and `data-sources/obot_mcp_tunnels/` read
  metadata and token previews.
- `guides/tunnel-aws/` and `guides/tunnel-vault/` publish a newly captured token.
  Use one destination example at a time and configure its provider credentials.
- `resources/obot_mcp_catalog_entry/` defines a remote catalog entry with fixed
  and user-supplied headers, with both import forms. `guides/tunnel-catalog-entry/`
  puts a tunnel behind a catalog entry and grants access to it.

The resource/data-source/guide examples are module snippets. Your root module
must declare `obot-platform/obot` as the Obot provider source. The
[tunnel guide](../docs/guides/mcp-tunnels.md) explains import, missing tokens,
rotation downtime, and state protection.

- `resources/obot_mcp_catalog_source/`, `resources/obot_git_credential/`, and
  `resources/obot_mcp_access_rule/` show source ownership, private sources, and
  independent MCP access policies, with import examples.
- `data-sources/obot_mcp_catalog/`, `obot_git_credential/`, `obot_git_credentials/`,
  `obot_mcp_access_rule/`, `obot_mcp_access_rules/`, and
  `obot_current_auth_provider/` provide discovery.
- `guides/catalog-adoption/` and `guides/access-policy-adoption/` adopt existing
  configuration. Copy live values before applying; see the
  [catalog and access-policy guide](../docs/guides/catalogs-and-access-policies.md)
  for all five workflows and deletion semantics. A plain creation example
  and an import file in the same resource directory are alternatives: adjust
  the resource configuration to match live values before importing.
