resource "obot_mcp_tunnel" "customer" {
  display_name = "Customer private network"
  # Must allow the entry's URL, as the tunnel agent's network sees it.
  allowed_urls = ["https://mcp.internal.example/mcp"]
}

resource "obot_mcp_catalog_entry" "customer" {
  name              = "Customer knowledge base"
  short_description = "Private MCP server reached through the customer's tunnel"

  remote = {
    fixed_url = "https://mcp.internal.example/mcp"
    tunnel_id = obot_mcp_tunnel.customer.id
  }
}

resource "obot_mcp_access_rule" "customer" {
  display_name = "Customer knowledge base users"
  subjects = [{
    type = "obotGroup"
    id   = "admin"
  }]
  resources = [{
    type = "mcpServerCatalogEntry"
    id   = obot_mcp_catalog_entry.customer.id
  }]
}
