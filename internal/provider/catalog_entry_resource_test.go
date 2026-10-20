// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"github.com/obot-platform/terraform-provider-obot/internal/client"
)

const catalogEntryAddress = "obot_mcp_catalog_entry.test"

// testAccCatalogEntryConfig defines a tunnel, an entry reached through it,
// and a policy granting administrators the entry.
func testAccCatalogEntryConfig(name, value string, version int) string {
	return fmt.Sprintf(`provider "obot" {}
resource "obot_mcp_tunnel" "test" {
  display_name = %[1]q
  allowed_urls = ["https://search.internal.example/mcp"]
}
resource "obot_mcp_catalog_entry" "test" {
  name              = %[1]q
  short_description = "Terraform acceptance test entry"
  metadata          = { categories = "Search" }
  remote = {
    fixed_url = "https://search.internal.example/mcp"
    tunnel_id = obot_mcp_tunnel.test.id
  }
  headers = [
    {
      key              = "X-API-KEY"
      name             = "API key"
      value_wo         = %[2]q
      value_wo_version = %[3]d
    },
    {
      key       = "X-USER-TOKEN"
      name      = "Your token"
      required  = true
      sensitive = true
      prefix    = "Bearer "
    },
  ]
}
resource "obot_mcp_access_rule" "test" {
  display_name = %[1]q
  subjects     = [{ type = "obotGroup", id = "admin" }]
  resources    = [{ type = "mcpServerCatalogEntry", id = obot_mcp_catalog_entry.test.id }]
}
`, name, value, version)
}

// testAccCatalogEntryMatches checks the entry in Obot, including the stored header value.
func testAccCatalogEntryMatches(t *testing.T, name, value string) statecheck.StateCheck {
	return tunnelStateCheck{address: catalogEntryAddress, check: func(ctx context.Context, values map[string]any) error {
		id, _ := values["id"].(string)
		entry, err := testAccTunnelClient(t).GetCatalogEntry(ctx, "default", id)
		if err != nil {
			return err
		}
		remote, _ := values["remote"].(map[string]any)
		if entry.Manifest.Name != name || !entry.Editable || entry.Manifest.RemoteConfig == nil ||
			entry.Manifest.RemoteConfig.TunnelName != remote["tunnel_id"] || len(entry.Manifest.Config) != 2 ||
			!entry.Manifest.Config[0].Static || entry.Manifest.Config[1].Static || entry.Manifest.Config[1].Prefix != "Bearer " {
			return fmt.Errorf("entry in Obot = %+v", entry)
		}
		var revealed map[string]string
		if err := testAccAPI(ctx, http.MethodPost, "/mcp-catalogs/default/entries/"+id+"/reveal", map[string]any{}, &revealed); err != nil {
			return err
		}
		if revealed["X-API-KEY"] != value {
			return fmt.Errorf("stored header value differs from the configured one")
		}
		return nil
	}}
}

func testAccCatalogEntryDestroy(t *testing.T) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		if err := testAccRuleDestroy(t)(state); err != nil {
			return err
		}
		for _, item := range state.RootModule().Resources {
			if item.Type == "obot_mcp_catalog_entry" {
				_, err := testAccTunnelClient(t).GetCatalogEntry(t.Context(), "default", item.Primary.ID)
				if !client.IsNotFound(err) {
					return fmt.Errorf("catalog entry still exists or absence could not be verified: %v", err)
				}
			}
		}
		return testAccTunnelDestroy(t)(state)
	}
}

func TestAccMCPCatalogEntry_tunnel(t *testing.T) {
	// Serial: the access rule shares the default catalog with other policy tests.
	name := "terraform-" + acctest.RandStringFromCharSet(8, acctest.CharSetAlphaNum)
	var id string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks:   testAccWriteOnlyVersion,
		CheckDestroy:             testAccCatalogEntryDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccCatalogEntryConfig(name, "first-value", 1),
				ConfigStateChecks: []statecheck.StateCheck{
					testAccCatalogEntryMatches(t, name, "first-value"),
					tunnelStateCheck{address: catalogEntryAddress, check: func(_ context.Context, v map[string]any) error {
						id, _ = v["id"].(string)
						return nil
					}},
					statecheck.ExpectKnownValue(catalogEntryAddress, tfjsonpath.New("catalog_id"), knownvalue.StringExact("default")),
					statecheck.ExpectKnownValue(catalogEntryAddress, tfjsonpath.New("headers").AtSliceIndex(0).AtMapKey("value_wo"), knownvalue.Null()),
					statecheck.ExpectIdentity(catalogEntryAddress, map[string]knownvalue.Check{"id": knownvalue.NotNull()}),
					testAccRuleExists(t, nil),
				},
			},
			// Renaming keeps the ID and the stored value, which is not resent.
			{
				Config: testAccCatalogEntryConfig(name+"-renamed", "ignored-value", 1),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(catalogEntryAddress, plancheck.ResourceActionUpdate),
				}},
				ConfigStateChecks: []statecheck.StateCheck{
					testAccCatalogEntryMatches(t, name+"-renamed", "first-value"),
					tunnelStateCheck{address: catalogEntryAddress, check: func(_ context.Context, v map[string]any) error {
						if v["id"] != id {
							return fmt.Errorf("renaming changed the entry ID")
						}
						return nil
					}},
				},
			},
			{
				Config:            testAccCatalogEntryConfig(name+"-renamed", "second-value", 2),
				ConfigStateChecks: []statecheck.StateCheck{testAccCatalogEntryMatches(t, name+"-renamed", "second-value")},
			},
			// A header value changed outside Terraform, as in the Obot UI, is
			// detected by its new revision and restored.
			{
				PreConfig: func() {
					conn := testAccTunnelClient(t)
					entry, err := conn.GetCatalogEntry(t.Context(), "default", id)
					if err != nil {
						t.Fatal(err)
					}
					manifest := entry.Manifest
					manifest.Config[0].Static, manifest.Config[0].Value = false, "changed-outside"
					if _, err := conn.UpdateCatalogEntry(t.Context(), "default", id, manifest); err != nil {
						t.Fatal(err)
					}
				},
				Config: testAccCatalogEntryConfig(name+"-renamed", "second-value", 2),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(catalogEntryAddress, plancheck.ResourceActionUpdate),
				}},
				ConfigStateChecks: []statecheck.StateCheck{testAccCatalogEntryMatches(t, name+"-renamed", "second-value")},
			},
			{
				ResourceName: catalogEntryAddress, ImportState: true, ImportStateVerify: true,
				ImportStateIdFunc:       func(*terraform.State) (string, error) { return "default/" + id, nil },
				ImportStateVerifyIgnore: []string{"headers.0.value_wo_version"},
			},
			// Reading an entry never returns its header values, so an import plans to send the configured one.
			{
				ResourceName: catalogEntryAddress, ImportState: true, ImportStateKind: resource.ImportBlockWithResourceIdentity, ExpectNonEmptyPlan: true,
				ImportPlanChecks: resource.ImportPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(catalogEntryAddress, plancheck.ResourceActionUpdate)}},
			},
			// A refreshed destroy plan removes all three, and applying it
			// succeeds only if the entry is deleted before its tunnel.
			{
				Config:  testAccCatalogEntryConfig(name+"-renamed", "second-value", 2),
				Destroy: true,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("obot_mcp_access_rule.test", plancheck.ResourceActionDestroy),
					plancheck.ExpectResourceAction(catalogEntryAddress, plancheck.ResourceActionDestroy),
					plancheck.ExpectResourceAction("obot_mcp_tunnel.test", plancheck.ResourceActionDestroy),
				}},
				Check: testAccCatalogEntryDestroy(t),
			},
		},
	})
}

// TestAccMCPCatalogEntry_unmodeledSettings adds configuration the resource
// cannot represent outside Terraform, as the Obot UI can, and checks that an
// update is refused without changing Obot and that a destroy still works.
func TestAccMCPCatalogEntry_unmodeledSettings(t *testing.T) {
	name := "terraform-" + acctest.RandStringFromCharSet(8, acctest.CharSetAlphaNum)
	var id string
	entry := func() *client.CatalogEntryDefinition {
		entry, err := testAccTunnelClient(t).GetCatalogEntry(t.Context(), "default", id)
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks:   testAccWriteOnlyVersion,
		CheckDestroy:             testAccCatalogEntryDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccCatalogEntryConfig(name, "first-value", 1),
				ConfigStateChecks: []statecheck.StateCheck{tunnelStateCheck{address: catalogEntryAddress, check: func(_ context.Context, v map[string]any) error {
					id, _ = v["id"].(string)
					return nil
				}}},
			},
			// Changing the entry is refused, and Obot keeps the outside setting.
			{
				PreConfig: func() {
					manifest := entry().Manifest
					manifest.Config = append(manifest.Config, client.CatalogEntryConfig{Key: "TENANT", Name: "Tenant", Usage: "interpolated"})
					if _, err := testAccTunnelClient(t).UpdateCatalogEntry(t.Context(), "default", id, manifest); err != nil {
						t.Fatal(err)
					}
					if unmodeled := entry().Unmodeled; len(unmodeled) != 1 {
						t.Fatalf("setup did not add one unmodeled setting: %q", unmodeled)
					}
				},
				Config:      testAccCatalogEntryConfig(name+"-renamed", "first-value", 1),
				ExpectError: regexp.MustCompile("Settings Terraform Cannot Manage"),
			},
			{
				PreConfig: func() {
					if current := entry(); current.Manifest.Name != name || len(current.Unmodeled) != 1 {
						t.Fatalf("refused update changed Obot: %+v", current)
					}
				},
				Config:  testAccCatalogEntryConfig(name, "first-value", 1),
				Destroy: true,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(catalogEntryAddress, plancheck.ResourceActionDestroy),
					plancheck.ExpectResourceAction("obot_mcp_tunnel.test", plancheck.ResourceActionDestroy),
				}},
				Check: testAccCatalogEntryDestroy(t),
			},
		},
	})
}

func TestAccMCPCatalogEntry_validation(t *testing.T) {
	config := func(remote, headers string) string {
		return fmt.Sprintf(`provider "obot" {}
resource "obot_mcp_catalog_entry" "test" {
  name    = "terraform invalid"
  remote  = %s
  headers = %s
}
`, remote, headers)
	}
	resource.Test(t, resource.TestCase{PreCheck: func() { testAccPreCheck(t) }, ProtoV6ProviderFactories: testAccProtoV6ProviderFactories, Steps: []resource.TestStep{
		{Config: config(`{ fixed_url = "https://a.example/mcp", hostname = "a.example" }`, `[]`), ExpectError: regexp.MustCompile("Invalid Attribute Combination")},
		{Config: config(`{ fixed_url = "https://a.example/mcp" }`, `[{ key = "x_api_key" }]`), ExpectError: regexp.MustCompile(`Use "X-API-KEY"`)},
		{Config: config(`{ fixed_url = "https://a.example/mcp" }`, `[{ key = "X-A", value_wo_version = 1 }]`), ExpectError: regexp.MustCompile("Invalid Attribute Combination")},
	}})
}

// TestAccMCPCatalogEntry_sourceEntryRejected imports an entry synchronized
// from a Git source, which this resource must refuse to manage.
func TestAccMCPCatalogEntry_sourceEntryRejected(t *testing.T) {
	if os.Getenv("TF_ACC") != "1" {
		t.Skip("acceptance tests require TF_ACC=1")
	}
	testAccPreCheck(t)
	entries, err := testAccTunnelClient(t).ListCatalogEntries(t.Context(), "default")
	if err != nil {
		t.Fatal(err)
	}
	var synced string
	for _, entry := range entries {
		if entry.SourceURL != "" {
			synced = entry.ID
			break
		}
	}
	if synced == "" {
		t.Skip("the default catalog has no source-managed entries")
	}
	resource.Test(t, resource.TestCase{PreCheck: func() { testAccPreCheck(t) }, ProtoV6ProviderFactories: testAccProtoV6ProviderFactories, Steps: []resource.TestStep{
		{Config: fmt.Sprintf(`provider "obot" {}
import {
  to = obot_mcp_catalog_entry.test
  id = %q
}
resource "obot_mcp_catalog_entry" "test" {
  name   = "terraform adoption"
  remote = { fixed_url = "https://a.example/mcp" }
}
`, synced), ExpectError: regexp.MustCompile("Managed by a Catalog Source")},
	}})
}
