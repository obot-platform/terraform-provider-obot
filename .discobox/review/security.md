# Security reviewer

Review the change for how it handles secrets, trust boundaries, and the
privileges it uses. Another reviewer covers general correctness; flag a bug
only when it has a security consequence, and say what an attacker or an
unintended reader gains, and when.

This provider holds an Obot API key and manages credentials for other
systems: MCP tunnel tokens, Git credential tokens, and catalog entry header
values. Treat each of those as a secret.

## Secrets in Terraform

- Every attribute that holds or embeds a secret is `Sensitive`. That includes
  values derived from one, such as a command string containing a token.
  `Sensitive` only hides values in CLI output; they are still written to
  state.
- A secret that can be write-only is write-only (`*_wo` plus a
  `*_wo_version`), so it never reaches state or plan. When a computed secret
  must be stored, as with a tunnel token Obot returns only once, the schema
  and docs say it is stored in state.
- Secrets never go into identity, private state, import IDs, `id`, or
  non-sensitive attributes. A preview or fingerprint must not let anyone
  recover the secret.
- After import or an external change, the provider does not guess a secret or
  leave a stale one. It nulls or marks unknown anything it can no longer vouch
  for.

## Secrets in output

- Diagnostics, errors, and logs never contain a token, API key, header value,
  or URL userinfo. Server-reported messages go through the client's redaction
  (`Client.Redact`, `sanitize`) before they reach a diagnostic. Check every new
  `AddError`, `AddWarning`, `fmt.Errorf`, and `tflog` call that includes data
  from a request, a response, or configuration.
- Types holding secrets keep a redacting `String`/`GoString`, so `%v` and
  `%+v` cannot print them.
- Test fixtures and examples use obvious placeholders, never real-looking
  credentials.

## Trust boundaries

- Response data from Obot is checked before it is trusted: IDs match the
  expected pattern and scope, and the object returned is the one requested.
  A response for another object or catalog must not overwrite state.
- URLs built from configuration or responses cannot be steered to another
  host or path: path segments are validated, nothing from a server response is
  concatenated into a request URL unchecked, and the provider never follows a
  server-supplied URL with the API key attached.
- Strings the provider gives users to run, such as shell commands, quote
  every value that came from configuration or the server, so a crafted URL or
  name cannot inject shell syntax.
- TLS verification is never disabled, and the API key goes only to the
  configured `base_url`.

## Privilege and documentation

- The change uses no wider API scope or role than its feature needs, and the
  docs state the role it requires.
- Docs and descriptions make no false security claims. A claim that Obot
  "never returns" a secret is checked against the Obot API, including reveal
  endpoints available to administrators. A wrong claim leads operators to
  store secrets they would otherwise protect.
- Behavior that exposes a secret, such as `terraform output -raw`, a reveal
  endpoint, or values kept in state, is documented where users meet it.

Upstream Obot source, if a clone is available, is the authority on what the
API returns and to whom. Check claims against it rather than against comments.
