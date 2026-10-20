// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"github.com/obot-platform/terraform-provider-obot/internal/client"
)

const tunnelAddress = "obot_mcp_tunnel.test"

type tunnelStateCheck struct {
	address string
	check   func(context.Context, map[string]any) error
}

func (c tunnelStateCheck) CheckState(ctx context.Context, req statecheck.CheckStateRequest, resp *statecheck.CheckStateResponse) {
	if req.State != nil && req.State.Values != nil && req.State.Values.RootModule != nil {
		for _, item := range req.State.Values.RootModule.Resources {
			if item.Address == c.address {
				resp.Error = c.check(ctx, item.AttributeValues)
				return
			}
		}
	}
	resp.Error = fmt.Errorf("expected resource missing from state: %s", c.address)
}

func testAccTunnelClient(t *testing.T) *client.Client {
	t.Helper()
	testAccPreCheck(t)
	conn, err := client.New(os.Getenv("OBOT_BASE_URL"), os.Getenv("OBOT_TOKEN"), "test")
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func testAccTunnelConfig(name, description, urls string, version int) string {
	return fmt.Sprintf(`provider "obot" {}
resource "obot_mcp_tunnel" "test" {
  display_name = %q
  description = %q
  allowed_urls = %s
  token_rotation_version = %d
}
`, name, description, urls, version)
}

func testAccTunnelDestroy(t *testing.T) resource.TestCheckFunc {
	t.Helper()
	return func(state *terraform.State) error {
		conn := testAccTunnelClient(t)
		for _, item := range state.RootModule().Resources {
			if item.Type != "obot_mcp_tunnel" {
				continue
			}
			_, err := conn.GetTunnel(t.Context(), item.Primary.ID)
			if !client.IsNotFound(err) {
				return fmt.Errorf("tunnel still exists or absence could not be verified: %v", err)
			}
		}
		return nil
	}
}

// testAccTunnelCommandsMatch checks that the agent commands carry the token in
// state, and are null exactly when it is.
func testAccTunnelCommandsMatch(baseURL string, values map[string]any) error {
	token, ok := values["token"].(string)
	if !ok {
		if values["command"] != nil || values["docker_command"] != nil {
			return fmt.Errorf("tunnel commands are set without a token")
		}
		return nil
	}
	args := "tunnel --obot-base-url " + baseURL + " --token " + token
	dockerCommand, _ := values["docker_command"].(string)
	if values["command"] != "obot "+args ||
		!regexp.MustCompile(`^docker run --rm ghcr\.io/obot-platform/obot:[^ ]+ `).MatchString(dockerCommand) ||
		!strings.HasSuffix(dockerCommand, " "+args) {
		return fmt.Errorf("tunnel commands do not match the token in state")
	}
	return nil
}

func testAccTunnelChecks(t *testing.T, extra func(context.Context, map[string]any) error) []statecheck.StateCheck {
	t.Helper()
	return []statecheck.StateCheck{
		statecheck.ExpectSensitiveValue(tunnelAddress, tfjsonpath.New("token")),
		statecheck.ExpectSensitiveValue(tunnelAddress, tfjsonpath.New("command")),
		statecheck.ExpectSensitiveValue(tunnelAddress, tfjsonpath.New("docker_command")),
		tunnelStateCheck{address: tunnelAddress, check: func(ctx context.Context, values map[string]any) error {
			id, _ := values["id"].(string)
			remote, err := testAccTunnelClient(t).GetTunnel(ctx, id)
			if err != nil {
				return err
			}
			if remote.Manifest.DisplayName != values["display_name"] || remote.Manifest.Description != values["description"] {
				return fmt.Errorf("API metadata does not match state")
			}
			urls, ok := values["allowed_urls"].([]any)
			if !ok || len(urls) != len(remote.Manifest.AllowedURLs) {
				return fmt.Errorf("API allowed URLs do not match state")
			}
			allowed := make(map[string]bool, len(urls))
			for _, value := range urls {
				rule, ok := value.(string)
				if !ok {
					return fmt.Errorf("invalid URL rule in state")
				}
				allowed[rule] = true
			}
			for _, value := range remote.Manifest.AllowedURLs {
				if !allowed[value] {
					return fmt.Errorf("API allowed URLs do not match state")
				}
			}
			if values["token_preview"] != remote.Token {
				return fmt.Errorf("API token preview does not match state")
			}
			if token, ok := values["token"].(string); ok {
				preview, err := client.TunnelTokenPreview(token)
				if err != nil || preview != remote.Token {
					return fmt.Errorf("state does not contain the current complete token")
				}
			}
			if err := testAccTunnelCommandsMatch(testAccTunnelClient(t).BaseURL(), values); err != nil {
				return err
			}
			if extra != nil {
				return extra(ctx, values)
			}
			return nil
		}},
	}
}

func TestAccMCPTunnel_lifecycle(t *testing.T) {
	var id, token string
	initial := testAccTunnelConfig("terraform lifecycle", "initial", `["http://internal/*", "*.example.com"]`, 0)
	cleared := testAccTunnelConfig("terraform lifecycle", "", `["http://internal/*", "*.example.com"]`, 0)
	updated := testAccTunnelConfig("renamed", "updated", `[]`, 0)
	rotated := testAccTunnelConfig("renamed", "updated", `[]`, 1)
	resource.ParallelTest(t, resource.TestCase{
		PreCheck: func() { testAccPreCheck(t) }, ProtoV6ProviderFactories: testAccProtoV6ProviderFactories, CheckDestroy: testAccTunnelDestroy(t),
		Steps: []resource.TestStep{
			{Config: initial, ConfigStateChecks: testAccTunnelChecks(t, func(_ context.Context, v map[string]any) error {
				id, _ = v["id"].(string)
				token, _ = v["token"].(string)
				if token == "" {
					return fmt.Errorf("create did not capture token")
				}
				return nil
			})},
			{Config: cleared, ConfigStateChecks: append(testAccTunnelChecks(t, nil),
				statecheck.ExpectKnownValue(tunnelAddress, tfjsonpath.New("description"), knownvalue.StringExact("")),
				statecheck.ExpectIdentity(tunnelAddress, map[string]knownvalue.Check{"id": knownvalue.NotNull()}),
			)},
			{Config: updated, ConfigStateChecks: testAccTunnelChecks(t, func(_ context.Context, v map[string]any) error {
				if v["id"] != id || v["token"] != token {
					return fmt.Errorf("metadata update changed ID or credential")
				}
				return nil
			})},
			{Config: updated, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}},
			{ResourceName: tunnelAddress, ImportState: true, ImportStateKind: resource.ImportCommandWithID, ImportStateVerify: true, ImportStateVerifyIgnore: []string{"token", "command", "docker_command"}},
			{ResourceName: tunnelAddress, ImportState: true, ImportStateKind: resource.ImportBlockWithID},
			{ResourceName: tunnelAddress, ImportState: true, ImportStateKind: resource.ImportBlockWithResourceIdentity},
			{Config: rotated, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectUnknownValue(tunnelAddress, tfjsonpath.New("token")),
				plancheck.ExpectUnknownValue(tunnelAddress, tfjsonpath.New("command")),
				plancheck.ExpectUnknownValue(tunnelAddress, tfjsonpath.New("docker_command")),
			}}, ConfigStateChecks: testAccTunnelChecks(t, func(_ context.Context, v map[string]any) error {
				if v["id"] != id || v["token"] == token {
					return fmt.Errorf("rotation did not change only the credential")
				}
				token, _ = v["token"].(string)
				return nil
			})},
			{Config: rotated, ConfigStateChecks: testAccTunnelChecks(t, func(_ context.Context, v map[string]any) error {
				if v["token"] != token {
					return fmt.Errorf("unchanged rotation version rotated again")
				}
				return nil
			})},
			{ResourceName: tunnelAddress, ImportState: true, ImportStateKind: resource.ImportCommandWithID, ImportStateVerify: true, ImportStateVerifyIgnore: []string{"token", "token_rotation_version", "command", "docker_command"}, ImportStateCheck: func(states []*terraform.InstanceState) error {
				if len(states) != 1 || states[0].Attributes["token"] != "" || states[0].Attributes["command"] != "" || states[0].Attributes["docker_command"] != "" || states[0].Attributes["token_rotation_version"] != "0" {
					return fmt.Errorf("CLI import did not leave the token unavailable and rotation version at zero")
				}
				return nil
			}},
			{Config: updated, ExpectError: regexp.MustCompile("Tunnel Rotation Version Cannot Decrease")},
		},
	})
}

func TestAccMCPTunnel_importAdoption(t *testing.T) {
	if os.Getenv("TF_ACC") != "1" {
		t.Skip("acceptance tests require TF_ACC=1")
	}
	conn := testAccTunnelClient(t)
	existing, err := conn.CreateTunnel(t.Context(), client.TunnelManifest{DisplayName: "adopt existing", AllowedURLs: []string{"http://internal/*"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.DeleteTunnel(context.Background(), existing.ID); err != nil {
			t.Error(err)
		}
	})
	stop := testAccTunnelAgent(t, existing.ID, existing.Token)
	defer stop()
	config := testAccTunnelConfig("adopt existing", "", `["http://internal/*"]`, 0)
	importConfig := config + fmt.Sprintf("\nimport {\n to = obot_mcp_tunnel.test\n id = %q\n}\n", existing.ID)
	assertImported := testAccTunnelChecks(t, func(ctx context.Context, v map[string]any) error {
		preview, _ := client.TunnelTokenPreview(existing.Token)
		if v["id"] != existing.ID || v["token"] != nil || v["token_preview"] != preview || fmt.Sprint(v["token_rotation_version"]) != "0" {
			return fmt.Errorf("import changed credentials or did not leave token null and rotation version zero")
		}
		return testAccTunnelConnection(ctx, existing.ID, true)
	})
	resource.ParallelTest(t, resource.TestCase{
		PreCheck: func() { testAccPreCheck(t) }, ProtoV6ProviderFactories: testAccProtoV6ProviderFactories, CheckDestroy: testAccTunnelDestroy(t),
		Steps: []resource.TestStep{
			{Config: importConfig, ConfigStateChecks: assertImported},
			{Config: config, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}},
			{Config: config + `
resource "terraform_data" "export" {
  input = obot_mcp_tunnel.test.token
  lifecycle {
    precondition {
      condition = obot_mcp_tunnel.test.token != null
      error_message = "Rotate the imported tunnel before exporting its token."
    }
  }
}`, ExpectError: regexp.MustCompile("Rotate the imported tunnel before exporting its token")},
			{Config: testAccTunnelConfig("adopt existing", "metadata after import", `["http://internal/*"]`, 0), ConfigStateChecks: assertImported},
			{Config: testAccTunnelConfig("adopt existing", "metadata after import", `["http://internal/*"]`, 1), ConfigStateChecks: testAccTunnelChecks(t, func(ctx context.Context, v map[string]any) error {
				if v["id"] != existing.ID || v["token"] == nil || v["token"] == existing.Token {
					return fmt.Errorf("post-import rotation failed")
				}
				if err := testAccTunnelConnection(ctx, existing.ID, false); err != nil {
					return err
				}
				stop()
				// The revoked credential cannot reconnect; the new one can.
				if testAccTunnelProbe(t, existing.ID, existing.Token) == nil {
					return fmt.Errorf("revoked token connected")
				}
				newToken, _ := v["token"].(string)
				newStop := testAccTunnelAgent(t, existing.ID, newToken)
				newStop()
				return nil
			})},
		},
	})
}

func TestAccMCPTunnel_drift(t *testing.T) {
	var id, token string
	config := testAccTunnelConfig("terraform drift", "managed", `["http://internal/*"]`, 0)
	checks := testAccTunnelChecks(t, func(_ context.Context, v map[string]any) error {
		id, _ = v["id"].(string)
		token, _ = v["token"].(string)
		return nil
	})
	resource.ParallelTest(t, resource.TestCase{
		PreCheck: func() { testAccPreCheck(t) }, ProtoV6ProviderFactories: testAccProtoV6ProviderFactories, CheckDestroy: testAccTunnelDestroy(t),
		Steps: []resource.TestStep{
			{Config: config, ConfigStateChecks: checks},
			{Config: config, PreConfig: func() {
				_, err := testAccTunnelClient(t).UpdateTunnel(t.Context(), id, client.TunnelManifest{DisplayName: "external", Description: "drift", AllowedURLs: []string{}})
				if err != nil {
					t.Fatal(err)
				}
			}, ConfigStateChecks: testAccTunnelChecks(t, func(_ context.Context, v map[string]any) error {
				if v["token"] != token {
					return fmt.Errorf("drift correction rotated token")
				}
				return nil
			})},
			{Config: config, PreConfig: func() {
				if _, err := testAccTunnelClient(t).RotateTunnelToken(t.Context(), id); err != nil {
					t.Fatal(err)
				}
			}, ConfigStateChecks: testAccTunnelChecks(t, func(_ context.Context, v map[string]any) error {
				if v["token"] != nil {
					return fmt.Errorf("external rotation left a stale token")
				}
				return nil
			})},
			{Config: testAccTunnelConfig("terraform drift", "managed", `["http://internal/*"]`, 1), ConfigStateChecks: checks},
			{Config: testAccTunnelConfig("terraform drift", "managed", `["http://internal/*"]`, 1), PreConfig: func() {
				if err := testAccTunnelClient(t).DeleteTunnel(t.Context(), id); err != nil {
					t.Fatal(err)
				}
			}, ConfigStateChecks: testAccTunnelChecks(t, func(_ context.Context, v map[string]any) error {
				if v["id"] == id || v["token"] == nil {
					return fmt.Errorf("deleted tunnel was not recreated")
				}
				return nil
			})},
		},
	})
}

func TestAccMCPTunnel_dataSources(t *testing.T) {
	config := testAccTunnelConfig("terraform lookup", "metadata", `["*.internal"]`, 0) + `
data "obot_mcp_tunnel" "one" { id = obot_mcp_tunnel.test.id }
data "obot_mcp_tunnels" "all" { depends_on = [obot_mcp_tunnel.test] }
`
	var id string
	resource.ParallelTest(t, resource.TestCase{
		PreCheck: func() { testAccPreCheck(t) }, ProtoV6ProviderFactories: testAccProtoV6ProviderFactories, CheckDestroy: testAccTunnelDestroy(t),
		Steps: []resource.TestStep{
			{Config: config, ConfigStateChecks: append(testAccTunnelChecks(t, func(_ context.Context, v map[string]any) error { id, _ = v["id"].(string); return nil }),
				statecheck.ExpectKnownValue("data.obot_mcp_tunnel.one", tfjsonpath.New("display_name"), knownvalue.StringExact("terraform lookup")),
				tunnelStateCheck{address: "data.obot_mcp_tunnels.all", check: func(_ context.Context, v map[string]any) error {
					items, ok := v["tunnels"].([]any)
					if !ok {
						return fmt.Errorf("tunnel list has incorrect shape")
					}
					found := false
					last := ""
					for _, item := range items {
						entry, ok := item.(map[string]any)
						if !ok {
							return fmt.Errorf("invalid list entry")
						}
						current, _ := entry["id"].(string)
						if current < last {
							return fmt.Errorf("list is not sorted")
						}
						last = current
						if current == id {
							found = true
						}
						if _, exists := entry["token"]; exists {
							return fmt.Errorf("metadata data source exposed full token")
						}
					}
					if !found {
						return fmt.Errorf("managed tunnel missing from list")
					}
					return nil
				}},
			)},
			{Config: config + `data "obot_mcp_tunnel" "missing" { id = "mt-does-not-exist" }`, ExpectError: regexp.MustCompile("MCP Tunnel Not Found")},
		},
	})
}

func TestAccMCPTunnel_invalidConfiguration(t *testing.T) {
	resource.ParallelTest(t, resource.TestCase{
		PreCheck: func() { testAccPreCheck(t) }, ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: testAccTunnelConfig("", "", `[]`, 0), ExpectError: regexp.MustCompile(`(?s)Invalid Attribute Value Match.*display_name`)},
			{Config: testAccTunnelConfig("name", "", `["bad*middle"]`, 0), ExpectError: regexp.MustCompile("Invalid Tunnel Allowed URL")},
			{Config: testAccTunnelConfig("name", "", `[]`, -1), ExpectError: regexp.MustCompile(`(?s)Invalid Attribute Value.*at least 0`)},
			{Config: testAccTunnelConfig("name", " padded ", `[]`, 0), ExpectError: regexp.MustCompile(`(?s)Invalid Attribute Value Match.*description`)},
			{Config: testAccTunnelConfig("name", "", `[]`, 0) + `import {
 to = obot_mcp_tunnel.test
 id = "../unsafe"
}`, ExpectError: regexp.MustCompile("Invalid MCP Tunnel Import ID")},
			{Config: testAccTunnelConfig("name", "", `[]`, 0) + `import {
 to = obot_mcp_tunnel.test
 id = "mt-does-not-exist"
}`, ExpectError: regexp.MustCompile("Cannot import non-existent remote object")},
		},
	})
}

func TestAccMCPTunnel_dependencies(t *testing.T) {
	var entryID, tunnelID string
	config := testAccTunnelConfig("terraform dependency", "", `["https://internal.example/mcp"]`, 0)
	cleanupEntry := func() {
		if entryID != "" {
			testAccTunnelAPI(t, http.MethodDelete, "/api/mcp-catalogs/default/entries/"+entryID, nil)
			entryID = ""
		}
	}
	t.Cleanup(func() {
		cleanupEntry()
		if tunnelID != "" {
			if err := testAccTunnelClient(t).DeleteTunnel(context.Background(), tunnelID); err != nil {
				t.Error(err)
			}
		}
	})
	resource.ParallelTest(t, resource.TestCase{
		PreCheck: func() { testAccPreCheck(t) }, ProtoV6ProviderFactories: testAccProtoV6ProviderFactories, CheckDestroy: testAccTunnelDestroy(t),
		Steps: []resource.TestStep{
			{Config: config, ConfigStateChecks: testAccTunnelChecks(t, func(_ context.Context, v map[string]any) error {
				id, _ := v["id"].(string)
				tunnelID = id
				entry := testAccTunnelAPI(t, http.MethodPost, "/api/mcp-catalogs/default/entries", map[string]any{"name": "terraform tunnel " + id, "runtime": "remote", "remoteConfig": map[string]any{"fixedURL": "https://internal.example/mcp", "tunnelName": id}})
				entryID, _ = entry["id"].(string)
				if entryID == "" {
					return fmt.Errorf("catalog entry creation returned no ID")
				}
				return nil
			})},
			{Config: testAccTunnelConfig("terraform dependency", "", `[]`, 0), ExpectError: regexp.MustCompile(`(?s)Error Updating MCP Tunnel.*HTTP 400.*cannot\s+be\s+updated\s+because\s+allowedURLs`)},
			{Config: `provider "obot" {}`, ExpectError: regexp.MustCompile(`(?s)Error Deleting MCP Tunnel.*HTTP 400.*cannot\s+be\s+deleted\s+because\s+it\s+is\s+used\s+by\s+MCP\s+server\s+catalog\s+entries`)},
			{PreConfig: cleanupEntry, Config: config},
		},
	})
}

func TestAccMCPTunnel_defaultsAndUnknownRotation(t *testing.T) {
	var id, token string
	config := `provider "obot" {}
resource "obot_mcp_tunnel" "test" {
  display_name = "terraform defaults"
  allowed_urls = ["https://one.example/mcp", "https://two.example/mcp"]
}
`
	unknown := `provider "obot" {}
resource "terraform_data" "version" { input = 1 }
resource "obot_mcp_tunnel" "test" {
  display_name = "terraform defaults"
  allowed_urls = ["https://two.example/mcp", "https://one.example/mcp"]
  token_rotation_version = terraform_data.version.output
}
`
	resource.ParallelTest(t, resource.TestCase{
		PreCheck: func() { testAccPreCheck(t) }, ProtoV6ProviderFactories: testAccProtoV6ProviderFactories, CheckDestroy: testAccTunnelDestroy(t),
		Steps: []resource.TestStep{
			{Config: config, ConfigStateChecks: append(testAccTunnelChecks(t, func(_ context.Context, v map[string]any) error {
				id, _ = v["id"].(string)
				token, _ = v["token"].(string)
				return nil
			}),
				statecheck.ExpectKnownValue(tunnelAddress, tfjsonpath.New("description"), knownvalue.StringExact("")),
				statecheck.ExpectKnownValue(tunnelAddress, tfjsonpath.New("token_rotation_version"), knownvalue.Int64Exact(0)))},
			{Config: strings.Replace(config, `["https://one.example/mcp", "https://two.example/mcp"]`, `["https://two.example/mcp", "https://one.example/mcp"]`, 1), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}},
			{Config: unknown, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectUnknownValue(tunnelAddress, tfjsonpath.New("token"))}}, ConfigStateChecks: testAccTunnelChecks(t, func(_ context.Context, v map[string]any) error {
				if v["id"] != id || v["token"] == token {
					return fmt.Errorf("unknown rotation trigger did not rotate in place")
				}
				return nil
			})},
		},
	})
}

func TestAccMCPTunnel_publicationFailure(t *testing.T) {
	var id, originalToken, failedPreview string
	config := testAccTunnelConfig("terraform publication retry", "", `[]`, 0)
	rotated := testAccTunnelConfig("terraform publication retry", "", `[]`, 1)
	resource.ParallelTest(t, resource.TestCase{
		PreCheck: func() { testAccPreCheck(t) }, ProtoV6ProviderFactories: testAccProtoV6ProviderFactories, CheckDestroy: testAccTunnelDestroy(t),
		Steps: []resource.TestStep{
			{Config: config, ConfigStateChecks: testAccTunnelChecks(t, func(_ context.Context, v map[string]any) error {
				id, _ = v["id"].(string)
				originalToken, _ = v["token"].(string)
				return nil
			})},
			{Config: rotated + `
# Simulate a dependent publisher failing after receiving the new token.
resource "terraform_data" "publisher" {
  input = obot_mcp_tunnel.test.token
  provisioner "local-exec" {
    command = "exit 1"
  }
}`, ExpectError: regexp.MustCompile("local-exec provisioner error")},
			{Config: rotated, PreConfig: func() {
				remote, err := testAccTunnelClient(t).GetTunnel(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				failedPreview = remote.Token
			}, ConfigStateChecks: testAccTunnelChecks(t, func(_ context.Context, v map[string]any) error {
				if v["id"] != id || v["token"] == nil || v["token"] == originalToken || v["token_preview"] != failedPreview || fmt.Sprint(v["token_rotation_version"]) != "1" {
					return fmt.Errorf("publication retry lost the captured token or rotated again")
				}
				return nil
			})},
		},
	})
}

func TestAccMCPTunnel_deleteConnected(t *testing.T) {
	var id, token string
	var stop func()
	resource.ParallelTest(t, resource.TestCase{
		PreCheck: func() { testAccPreCheck(t) }, ProtoV6ProviderFactories: testAccProtoV6ProviderFactories, CheckDestroy: testAccTunnelDestroy(t),
		Steps: []resource.TestStep{
			{Config: testAccTunnelConfig("terraform delete connected", "", `[]`, 0), ConfigStateChecks: testAccTunnelChecks(t, func(_ context.Context, v map[string]any) error {
				id, _ = v["id"].(string)
				token, _ = v["token"].(string)
				stop = testAccTunnelAgent(t, id, token)
				return nil
			})},
			{Config: `provider "obot" {}
data "obot_mcp_tunnels" "all" {}`, ConfigStateChecks: []statecheck.StateCheck{tunnelStateCheck{address: "data.obot_mcp_tunnels.all", check: func(ctx context.Context, _ map[string]any) error {
				if _, err := testAccTunnelClient(t).GetTunnel(ctx, id); !client.IsNotFound(err) {
					return fmt.Errorf("connected tunnel was not deleted")
				}
				if err := testAccTunnelConnection(ctx, id, false); err != nil {
					return err
				}
				stop()
				if testAccTunnelProbe(t, id, token) == nil {
					return fmt.Errorf("deleted tunnel's token reconnected")
				}
				return nil
			}}}},
		},
	})
}

func testAccTunnelAPI(t *testing.T, method, path string, body any) map[string]any {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal("cannot encode test fixture")
		}
	}
	base := strings.TrimSuffix(strings.TrimRight(os.Getenv("OBOT_BASE_URL"), "/"), "/api")
	// Fixtures also need cleanup after the testing context has been canceled.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("OBOT_TOKEN"))
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal("fixture request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("fixture %s %s returned HTTP %d", method, path, response.StatusCode)
	}
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil && err != io.EOF {
		t.Fatal("invalid fixture response")
	}
	return result
}

// Agent tests use the CLI paired with the instance by scripts/obot.py.
func testAccTunnelAgent(t *testing.T, id, token string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, "obot", "tunnel")
	command.Env = append(os.Environ(), "OBOT_TUNNEL_TOKEN="+token)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal("could not start the matching Obot CLI: ", err)
	}
	done := make(chan struct{})
	go func() { _ = command.Wait(); close(done) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	waitCtx, waitCancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer waitCancel()
	if err := testAccTunnelConnection(waitCtx, id, true); err != nil {
		stop()
		t.Fatal(err)
	}
	return stop
}

func testAccTunnelProbe(t *testing.T, id, token string) error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	command := exec.CommandContext(ctx, "obot", "tunnel")
	command.Env = append(os.Environ(), "OBOT_TUNNEL_TOKEN="+token)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = command.Wait() }()
	probeCtx, probeCancel := context.WithTimeout(ctx, 2*time.Second)
	defer probeCancel()
	return testAccTunnelConnection(probeCtx, id, true)
}

func testAccTunnelConnection(ctx context.Context, id string, want bool) error {
	// Bound waits even when a state check supplies an unbounded context.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	base := strings.TrimSuffix(strings.TrimRight(os.Getenv("OBOT_BASE_URL"), "/"), "/api")
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/tunnels", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+os.Getenv("OBOT_TOKEN"))
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("tunnel connection did not reach expected state")
			}
			return fmt.Errorf("checking connected tunnels failed")
		}
		var result struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
		}
		decodeErr := json.NewDecoder(response.Body).Decode(&result)
		_ = response.Body.Close()
		if response.StatusCode != 200 || decodeErr != nil {
			return fmt.Errorf("cannot inspect connected tunnels (HTTP %d)", response.StatusCode)
		}
		connected := false
		for _, item := range result.Items {
			if item.Name == id {
				connected = true
			}
		}
		if connected == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("tunnel connection did not reach expected state")
		case <-ticker.C:
		}
	}
}
