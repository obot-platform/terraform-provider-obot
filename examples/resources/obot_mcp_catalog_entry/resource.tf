variable "search_api_key" {
  description = "Key the MCP server requires from Obot."
  type        = string
  sensitive   = true
  ephemeral   = true
}

resource "obot_mcp_catalog_entry" "search" {
  name              = "Internal Search"
  short_description = "Search internal documents"
  icon              = "https://search.example.com/icon.png"
  metadata = {
    categories = "Search"
  }

  remote = {
    fixed_url = "https://search.example.com/mcp"
  }

  headers = [
    {
      # A fixed value, sent on every request and never shown to users.
      # Write-only: never stored in state. Increase the version to send a new value.
      key              = "X-API-KEY"
      value_wo         = var.search_api_key
      value_wo_version = 1
    },
    {
      # No value: each user supplies their own when connecting.
      key         = "AUTHORIZATION"
      name        = "Personal access token"
      description = "Create one under Settings > Tokens."
      required    = true
      sensitive   = true
      prefix      = "Bearer "
    },
  ]
}
