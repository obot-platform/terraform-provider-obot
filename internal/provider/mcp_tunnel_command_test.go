// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/obot-platform/terraform-provider-obot/internal/client"
)

func TestTunnelImageTag(t *testing.T) {
	for version, want := range map[string]string{
		"v0.15.0":               "v0.15.0",
		" v0.15.0 ":             "v0.15.0",
		"0.15.0":                "0.15.0",
		"v0.15.0+abc123":        "v0.15.0",
		"v1.2.3-rc.1+build.5":   "v1.2.3-rc.1",
		"v1.2.3-rc.1":           "v1.2.3-rc.1",
		"":                      "main",
		"v0.0.0-dev":            "main",
		"v0.0.0-dev-1a2b3c":     "main",
		"0.0.0-dev+1a2b3c":      "main",
		"v0.15.0-dirty":         "main",
		"v0.15.0-1-gabc-dirty":  "main",
		"latest":                "main",
		"v0.15":                 "main",
		"v0.15.0; rm -rf ~":     "main",
		"v0.15.0+bad metadata!": "main",
	} {
		if got := tunnelImageTag(version); got != want {
			t.Errorf("tunnelImageTag(%q) = %q, want %q", version, got, want)
		}
	}
}

func TestTunnelCommandsFormatting(t *testing.T) {
	token := tunnelTestToken(1)
	command, docker := tunnelCommands("https://obot.example.com/api", "v0.15.0", token)
	if command != "obot tunnel --obot-base-url https://obot.example.com/api --token "+token {
		t.Fatalf("unexpected command %q", command)
	}
	if docker != "docker run --rm ghcr.io/obot-platform/obot:v0.15.0 tunnel --obot-base-url https://obot.example.com/api --token "+token {
		t.Fatalf("unexpected docker command %q", docker)
	}
	for value, want := range map[string]string{
		"http://localhost:8080/api":     "http://localhost:8080/api",
		"https://h/a$b/api":             "'https://h/a$b/api'",
		"https://h/it's/api":            `'https://h/it'\''s/api'`,
		"http://[::1]:8080/api":         "'http://[::1]:8080/api'",
		"https://h/a;b&c/api":           "'https://h/a;b&c/api'",
		"https://h/%20space/api":        "https://h/%20space/api",
		"https://user.example.com/~x/a": "https://user.example.com/~x/a",
	} {
		if got := shellWord(value); got != want {
			t.Errorf("shellWord(%q) = %q, want %q", value, got, want)
		}
	}
}

// tunnelCommandServer fakes the tunnel API, returning the complete token for
// seed to creation and rotation POSTs and its preview otherwise, and answers /api/version with the given status and body.
// It counts version requests.
func tunnelCommandServer(t *testing.T, seed byte, versionStatus int, versionBody string) (*client.Client, *atomic.Int32) {
	t.Helper()
	var versionCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			versionCalls.Add(1)
			w.WriteHeader(versionStatus)
			_, _ = w.Write([]byte(versionBody))
			return
		}
		result := client.Tunnel{ID: "mt1", Manifest: client.TunnelManifest{DisplayName: "private", AllowedURLs: []string{"http://internal/*"}}}
		if r.Method == http.MethodPost {
			result.Token = tunnelTestToken(seed)
		} else {
			result.Token, _ = client.TunnelTokenPreview(tunnelTestToken(seed))
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	t.Cleanup(server.Close)
	conn, err := client.New(server.URL, "test-key", "test")
	if err != nil {
		t.Fatal(err)
	}
	return conn, &versionCalls
}

func tunnelCommandCreate(t *testing.T, conn *client.Client) (tunnelResourceModel, resource.CreateResponse) {
	t.Helper()
	model := tunnelTestModel()
	model.ID = types.StringUnknown()
	model.Token = types.StringUnknown()
	model.TokenPreview = types.StringUnknown()
	model.Command = types.StringUnknown()
	model.DockerCommand = types.StringUnknown()
	plan := tunnelTestState(t, model)
	response := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema}}
	(&mcpTunnelResource{client: conn}).Create(t.Context(), resource.CreateRequest{Plan: tfsdk.Plan(plan)}, &response)
	if d := response.State.Get(t.Context(), &model); d.HasError() {
		t.Fatal(d)
	}
	return model, response
}

func TestTunnelCreateCommands(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		wantTag    string
		warning    bool
	}{
		{"release", `{"obot":"v0.15.0"}`, 200, "v0.15.0", false},
		{"build metadata", `{"obot":"v0.15.0+3f2a1b"}`, 200, "v0.15.0", false},
		{"development", `{"obot":"v0.0.0-dev-3f2a1b"}`, 200, "main", false},
		{"dirty", `{"obot":"v0.15.0-dirty"}`, 200, "main", false},
		{"missing", `{}`, 200, "main", false},
		{"error", `failed`, 500, "main", true},
		{"forbidden", ``, 403, "main", true},
		{"malformed", `not json`, 200, "main", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn, _ := tunnelCommandServer(t, 1, test.status, test.body)
			model, response := tunnelCommandCreate(t, conn)
			if response.Diagnostics.HasError() || (len(response.Diagnostics.Warnings()) > 0) != test.warning {
				t.Fatal(response.Diagnostics)
			}
			args := " tunnel --obot-base-url " + conn.BaseURL() + " --token " + tunnelTestToken(1)
			if !strings.HasSuffix(conn.BaseURL(), "/api") || model.Command.ValueString() != "obot"+args {
				t.Fatalf("unexpected command %q", model.Command.ValueString())
			}
			if model.DockerCommand.ValueString() != "docker run --rm ghcr.io/obot-platform/obot:"+test.wantTag+args {
				t.Fatalf("unexpected docker command %q", model.DockerCommand.ValueString())
			}
		})
	}
}

func TestTunnelRotationUpdatesCommands(t *testing.T) {
	conn, _ := tunnelCommandServer(t, 2, 200, `{"obot":"v0.16.0"}`)
	model := tunnelTestModel()
	model.Command, model.DockerCommand = types.StringValue("old"), types.StringValue("old docker")
	state := tunnelTestState(t, model)
	model.TokenRotationVersion = types.Int64Value(1)
	model.Token, model.TokenPreview = types.StringUnknown(), types.StringUnknown()
	model.Command, model.DockerCommand = types.StringUnknown(), types.StringUnknown()
	plan := tunnelTestState(t, model)
	response := resource.UpdateResponse{State: state}
	(&mcpTunnelResource{client: conn}).Update(t.Context(), resource.UpdateRequest{State: state, Plan: tfsdk.Plan(plan)}, &response)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	var got tunnelResourceModel
	if d := response.State.Get(t.Context(), &got); d.HasError() {
		t.Fatal(d)
	}
	wantCommand, wantDocker := tunnelCommands(conn.BaseURL(), "v0.16.0", tunnelTestToken(2))
	if got.Command.ValueString() != wantCommand || got.DockerCommand.ValueString() != wantDocker {
		t.Fatalf("rotation did not update commands: %q, %q", got.Command.ValueString(), got.DockerCommand.ValueString())
	}
}

func TestTunnelMetadataUpdateKeepsCommands(t *testing.T) {
	conn, versionCalls := tunnelCommandServer(t, 1, 200, `{"obot":"v0.16.0"}`)
	model := tunnelTestModel()
	model.Command, model.DockerCommand = types.StringValue("captured"), types.StringValue("captured docker")
	state := tunnelTestState(t, model)
	model.DisplayName = types.StringValue("updated")
	plan := tunnelTestState(t, model)
	response := resource.UpdateResponse{State: state}
	(&mcpTunnelResource{client: conn}).Update(t.Context(), resource.UpdateRequest{State: state, Plan: tfsdk.Plan(plan)}, &response)
	var got tunnelResourceModel
	if d := response.State.Get(t.Context(), &got); d.HasError() || response.Diagnostics.HasError() {
		t.Fatal(d, response.Diagnostics)
	}
	if got.Command.ValueString() != "captured" || got.DockerCommand.ValueString() != "captured docker" || versionCalls.Load() != 0 {
		t.Fatal("metadata update changed the captured commands")
	}
}

func TestTunnelModifyPlanCommands(t *testing.T) {
	for _, test := range []struct {
		name     string
		next     int64
		imported bool
		unknown  bool
	}{
		{name: "metadata", next: 0},
		{name: "imported metadata", next: 0, imported: true},
		{name: "rotation", next: 1, unknown: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := tunnelTestModel()
			model.Command, model.DockerCommand = types.StringValue("cli"), types.StringValue("docker")
			if test.imported {
				model.Token, model.Command, model.DockerCommand = types.StringNull(), types.StringNull(), types.StringNull()
			}
			state := tunnelTestState(t, model)
			model.TokenRotationVersion = types.Int64Value(test.next)
			model.Token, model.TokenPreview = types.StringUnknown(), types.StringUnknown()
			model.Command, model.DockerCommand = types.StringUnknown(), types.StringUnknown()
			plan := tfsdk.Plan(tunnelTestState(t, model))
			response := resource.ModifyPlanResponse{Plan: plan}
			(&mcpTunnelResource{}).ModifyPlan(t.Context(), resource.ModifyPlanRequest{State: state, Plan: plan}, &response)
			if d := response.Plan.Get(t.Context(), &model); d.HasError() || response.Diagnostics.HasError() {
				t.Fatal(d, response.Diagnostics)
			}
			for _, value := range []types.String{model.Command, model.DockerCommand} {
				if value.IsUnknown() != test.unknown || (!test.unknown && value.IsNull() != test.imported) {
					t.Fatal("commands are not planned like the token")
				}
			}
			if !test.unknown && !test.imported && (model.Command.ValueString() != "cli" || model.DockerCommand.ValueString() != "docker") {
				t.Fatal("commands are not stable without rotation")
			}
		})
	}
}

func TestTunnelImportLeavesCommandsNull(t *testing.T) {
	conn, versionCalls := tunnelCommandServer(t, 1, 200, `{"obot":"v0.15.0"}`)
	r := &mcpTunnelResource{client: conn}
	response := resource.ImportStateResponse{State: tunnelTestState(t, tunnelResourceModel{})}
	r.ImportState(t.Context(), resource.ImportStateRequest{ID: "mt1"}, &response)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	read := resource.ReadResponse{State: response.State}
	r.Read(t.Context(), resource.ReadRequest{State: response.State}, &read)
	var model tunnelResourceModel
	if d := read.State.Get(t.Context(), &model); d.HasError() || read.Diagnostics.HasError() {
		t.Fatal(d, read.Diagnostics)
	}
	if !model.Token.IsNull() || !model.Command.IsNull() || !model.DockerCommand.IsNull() || versionCalls.Load() != 0 {
		t.Fatal("import produced commands without a token")
	}
}

func TestTunnelReadCommands(t *testing.T) {
	// captured is the base URL in the commands already in state; "" means none.
	for _, test := range []struct {
		name                string
		serverSeed          byte
		captured            string
		wantCommand         string
		wantNull            bool
		wantVersionRequests int32
	}{
		// A captured command keeps its image tag although the server version changed.
		{"stable", 1, "current", "kept", false, 0},
		// State written before these attributes existed gains them.
		{"missing", 1, "", "", false, 1},
		// A changed base_url rebuilds both commands.
		{"base URL changed", 1, "http://old.example/api", "", false, 1},
		{"external rotation", 9, "current", "", true, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn, versionCalls := tunnelCommandServer(t, test.serverSeed, 200, `{"obot":"v9.9.9"}`)
			model := tunnelTestModel()
			model.Command, model.DockerCommand = types.StringNull(), types.StringNull()
			if test.captured != "" {
				url := test.captured
				if url == "current" {
					url = conn.BaseURL()
				}
				command, docker := tunnelCommands(url, "v0.1.0", tunnelTestToken(1))
				model.Command, model.DockerCommand = types.StringValue(command), types.StringValue(docker)
			}
			if test.wantCommand == "kept" {
				test.wantCommand = model.DockerCommand.ValueString()
			}
			state := tunnelTestState(t, model)
			read := resource.ReadResponse{State: state}
			(&mcpTunnelResource{client: conn}).Read(t.Context(), resource.ReadRequest{State: state}, &read)
			if d := read.State.Get(t.Context(), &model); d.HasError() || read.Diagnostics.HasError() {
				t.Fatal(d, read.Diagnostics)
			}
			if test.wantCommand == "" && !test.wantNull {
				_, test.wantCommand = tunnelCommands(conn.BaseURL(), "v9.9.9", tunnelTestToken(1))
			}
			if model.Command.IsNull() != test.wantNull || model.DockerCommand.IsNull() != test.wantNull ||
				(!test.wantNull && model.DockerCommand.ValueString() != test.wantCommand) || versionCalls.Load() != test.wantVersionRequests {
				t.Fatalf("unexpected commands after read: %q (%d version requests)", model.DockerCommand.ValueString(), versionCalls.Load())
			}
		})
	}
}
