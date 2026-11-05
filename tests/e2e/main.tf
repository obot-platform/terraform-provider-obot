# End-to-end scenario for the compiled provider against a real Obot server.
#
# scripts/e2e.py drives this configuration with the Terraform CLI: create,
# confirm an empty plan, update, refresh, discard state and re-import every
# object (by ID and by identity), destroy, and verify Obot directly. The
# upgrade test applies it before an Obot upgrade and plans it afterwards.
#
# Postconditions make every data source assert what the API returned, so a
# changed field or shape fails the apply instead of passing silently.

terraform {
  required_version = ">= 1.12"
  required_providers {
    obot = {
      source = "obot-platform/obot"
    }
  }
}

provider "obot" {}

variable "expected_email" {
  description = "Email of the owner whose API key the provider uses."
  type        = string
}

variable "generation" {
  description = "Increase to update every managed object in place."
  type        = number
  default     = 1
}

variable "public_source" {
  description = "Public YAML catalog source served by the HTTPS fixture."
  type        = string
}

variable "public_entry" {
  description = "Catalog entry name the public source provides."
  type        = string
}

variable "private_source" {
  description = "Private Git catalog source served by the HTTPS fixture."
  type        = string
}

variable "private_entry" {
  description = "Catalog entry name the private source provides."
  type        = string
}

variable "private_host" {
  description = "Host of the private Git source, including any port."
  type        = string
}

variable "private_token" {
  description = "Token the private Git source requires."
  type        = string
  sensitive   = true
  ephemeral   = true
}

locals {
  name = "terraform-e2e-g${var.generation}"
}

resource "obot_git_credential" "private" {
  display_name     = "${local.name} private catalog"
  host             = var.private_host
  token_wo         = var.private_token
  token_wo_version = 1
}

resource "obot_mcp_catalog_source" "public" {
  url = var.public_source
}

resource "obot_mcp_catalog_source" "private" {
  url               = var.private_source
  git_credential_id = obot_git_credential.private.id
}

resource "obot_mcp_tunnel" "e2e" {
  display_name           = "${local.name} tunnel"
  description            = "End-to-end tunnel, generation ${var.generation}"
  allowed_urls           = var.generation == 1 ? ["https://mcp.internal.example/mcp"] : ["https://mcp.internal.example/mcp", "mcp-${var.generation}.internal.example"]
  token_rotation_version = 0
}

resource "obot_mcp_access_rule" "e2e" {
  display_name = "${local.name} access"
  subjects = var.generation == 1 ? [{ type = "obotGroup", id = "admin" }] : [
    { type = "obotGroup", id = "admin" },
    { type = "obotGroup", id = "owner" },
  ]
  resources = [{ type = "selector", id = "*" }]
}

data "obot_current_user" "me" {
  lifecycle {
    postcondition {
      condition     = self.email == var.expected_email && self.id != ""
      error_message = "obot_current_user returned ${self.email}, not ${var.expected_email}."
    }
  }
}

data "obot_current_auth_provider" "current" {
  lifecycle {
    postcondition {
      condition     = self.id != "" && self.namespace != ""
      error_message = "obot_current_auth_provider returned no provider."
    }
  }
}

data "obot_mcp_catalog" "default" {
  id         = "default"
  depends_on = [obot_mcp_catalog_source.public, obot_mcp_catalog_source.private]

  lifecycle {
    postcondition {
      condition = alltrue([
        contains(self.sources, var.public_source),
        contains(self.sources, var.private_source),
        lookup(self.source_git_credential_ids, var.private_source, "") == obot_git_credential.private.id,
        lookup(self.sync_errors, var.public_source, "") == "",
        lookup(self.sync_errors, var.private_source, "") == "",
      ])
      error_message = "The default catalog's sources, credentials, or sync errors differ from the managed sources."
    }
    postcondition {
      condition = alltrue([
        contains([for e in self.entries : e.name if e.source_url == var.public_source], var.public_entry),
        contains([for e in self.entries : e.name if e.source_url == var.private_source], var.private_entry),
      ])
      error_message = "The default catalog does not list the entries synced from the managed sources."
    }
  }
}

data "obot_git_credential" "by_id" {
  id = obot_git_credential.private.id

  lifecycle {
    postcondition {
      condition     = self.display_name == obot_git_credential.private.display_name && self.host == var.private_host && self.token_configured
      error_message = "obot_git_credential by ID differs from the managed credential."
    }
  }
}

data "obot_git_credential" "by_name" {
  display_name = obot_git_credential.private.display_name

  lifecycle {
    postcondition {
      condition     = self.id == obot_git_credential.private.id
      error_message = "obot_git_credential by display name did not find the managed credential."
    }
  }
}

data "obot_git_credentials" "all" {
  depends_on = [obot_git_credential.private]

  lifecycle {
    postcondition {
      condition     = contains([for c in self.credentials : c.id], obot_git_credential.private.id)
      error_message = "obot_git_credentials does not list the managed credential."
    }
  }
}

data "obot_mcp_tunnel" "e2e" {
  id = obot_mcp_tunnel.e2e.id

  lifecycle {
    postcondition {
      condition = alltrue([
        self.display_name == obot_mcp_tunnel.e2e.display_name,
        self.description == obot_mcp_tunnel.e2e.description,
        self.allowed_urls == obot_mcp_tunnel.e2e.allowed_urls,
        self.token_preview != "",
      ])
      error_message = "obot_mcp_tunnel differs from the managed tunnel."
    }
  }
}

data "obot_mcp_tunnels" "all" {
  depends_on = [obot_mcp_tunnel.e2e]

  lifecycle {
    postcondition {
      condition     = contains([for t in self.tunnels : t.id], obot_mcp_tunnel.e2e.id)
      error_message = "obot_mcp_tunnels does not list the managed tunnel."
    }
  }
}

data "obot_mcp_access_rule" "e2e" {
  id = obot_mcp_access_rule.e2e.id

  lifecycle {
    postcondition {
      condition = alltrue([
        self.display_name == obot_mcp_access_rule.e2e.display_name,
        self.subjects == obot_mcp_access_rule.e2e.subjects,
        self.resources == obot_mcp_access_rule.e2e.resources,
        !self.generated,
      ])
      error_message = "obot_mcp_access_rule differs from the managed rule."
    }
  }
}

data "obot_mcp_access_rules" "all" {
  depends_on = [obot_mcp_access_rule.e2e]

  lifecycle {
    postcondition {
      condition     = contains([for r in self.rules : r.id], obot_mcp_access_rule.e2e.id)
      error_message = "obot_mcp_access_rules does not list the managed rule."
    }
  }
}
