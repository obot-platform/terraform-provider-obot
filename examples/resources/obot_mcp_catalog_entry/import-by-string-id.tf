# Reading an entry never returns its header values, so the first apply after import sends
# each header's configured value_wo, replacing the stored value.
import {
  to = obot_mcp_catalog_entry.search
  id = "default-internal-search-abc12"
}
