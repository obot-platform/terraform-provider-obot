// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/obot-platform/terraform-provider-obot/internal/client"
)

func testManagementClient(t *testing.T, handler http.HandlerFunc) *client.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	conn, err := client.New(server.URL, "test-key", "test")
	if err != nil {
		t.Fatal(err)
	}
	return conn
}
func testManagementState(t *testing.T, r resource.Resource, model any) tfsdk.State {
	t.Helper()
	var schema resource.SchemaResponse
	r.Schema(t.Context(), resource.SchemaRequest{}, &schema)
	state := tfsdk.State{Schema: schema.Schema}
	if diagnostics := state.Set(t.Context(), model); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	return state
}
func testRuleModel(t *testing.T) accessRuleModel {
	t.Helper()
	var diagnostics diag.Diagnostics
	model := accessRuleModel{}
	model.fromAPI(t.Context(), &client.AccessRule{ID: "acr1-everything", CatalogID: "default", AccessRuleManifest: client.AccessRuleManifest{DisplayName: "Everything", Subjects: []client.RuleReference{{Type: "obotGroup", ID: "admin"}}, Resources: []client.RuleReference{{Type: "selector", ID: "*"}}}}, &diagnostics)
	if diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	return model
}
func TestManagementImportIdentifiers(t *testing.T) {
	for _, test := range []struct {
		name, id    string
		rule, valid bool
	}{
		{"source URL", "https://example.com/catalog.yaml", false, true}, {"catalog ID", "default", false, false}, {"plain HTTP", "http://example.com/catalog.yaml", false, false}, {"embedded credentials", "https://user:secret@example.com/catalog.yaml", false, false},
		{"policy", "default/acr1-everything", true, true}, {"bare policy ID", "acr1-everything", true, true}, {"other catalog", "other/acr1-everything", true, false}, {"empty ID", "default/", true, false}, {"extra slash", "default/../acr1", true, false}, {"query injection", "default/acr1?x=y", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var r resource.Resource
			var state tfsdk.State
			if test.rule {
				r = newMCPAccessRuleResource()
				state = testManagementState(t, r, testRuleModel(t))
			} else {
				r = newMCPCatalogSourceResource()
				state = testManagementState(t, r, testSourceModel())
			}
			response := resource.ImportStateResponse{State: state}
			importer, ok := r.(resource.ResourceWithImportState)
			if !ok {
				t.Fatal("missing import support")
			}
			importer.ImportState(t.Context(), resource.ImportStateRequest{ID: test.id}, &response)
			if response.Diagnostics.HasError() == test.valid {
				t.Fatal(response.Diagnostics)
			}
		})
	}
}
func TestManagementReadAndDeleteErrors(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 500} {
		for _, kind := range []string{"catalog", "rule"} {
			t.Run(kind+http.StatusText(status), func(t *testing.T) {
				conn := testManagementClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
				var r resource.Resource
				var state tfsdk.State
				if kind == "catalog" {
					r = &mcpCatalogSourceResource{client: conn}
					state = testManagementState(t, r, testSourceModel())
				} else {
					r = &mcpAccessRuleResource{client: conn}
					state = testManagementState(t, r, testRuleModel(t))
				}
				read := resource.ReadResponse{State: state}
				r.Read(t.Context(), resource.ReadRequest{State: state}, &read)
				if read.Diagnostics.HasError() != (status != 404) || read.State.Raw.IsNull() != (status == 404) {
					t.Fatalf("incorrect missing-object read: %v", read.Diagnostics)
				}
				deleted := resource.DeleteResponse{State: state}
				r.Delete(t.Context(), resource.DeleteRequest{State: state}, &deleted)
				if deleted.Diagnostics.HasError() != (status != 404) {
					t.Fatal(deleted.Diagnostics)
				}
			})
		}
	}
}
func TestGeneratedRuleCannotBeManaged(t *testing.T) {
	mutations := 0
	conn := testManagementClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			mutations++
		}
		rule := client.AccessRule{ID: "acr1-everything", CatalogID: "default", Generated: true}
		_ = json.NewEncoder(w).Encode(rule)
	})
	r := &mcpAccessRuleResource{client: conn}
	state := testManagementState(t, r, testRuleModel(t))
	read := resource.ReadResponse{State: state}
	r.Read(t.Context(), resource.ReadRequest{State: state}, &read)
	updated := resource.UpdateResponse{State: state}
	r.Update(t.Context(), resource.UpdateRequest{Plan: tfsdk.Plan(state), State: state}, &updated)
	deleted := resource.DeleteResponse{State: state}
	r.Delete(t.Context(), resource.DeleteRequest{State: state}, &deleted)
	if !read.Diagnostics.HasError() || !updated.Diagnostics.HasError() || !deleted.Diagnostics.HasError() || mutations != 0 {
		t.Fatal("generated rule was managed")
	}
}
func TestRuleReferenceValidation(t *testing.T) {
	for _, test := range []struct {
		kind, id        string
		subjects, valid bool
	}{
		{"selector", "*", true, true}, {"selector", "everyone", true, false}, {"user", "1", true, true}, {"group", "entra/group-id", true, true}, {"obotGroup", "admin", true, true}, {"profile", "foo", true, false}, {"user", "", true, false},
		{"selector", "*", false, true}, {"mcpServerCatalogEntry", "entry1", false, true}, {"mcpServer", "server1", false, true}, {"mcpCatalog", "default", false, false}, {"selector", "anything", false, false},
	} {
		t.Run(test.kind+test.id, func(t *testing.T) {
			var response validator.SetResponse
			value := types.SetValueMust(types.ObjectType{AttrTypes: referenceAttrTypes}, []attr.Value{types.ObjectValueMust(referenceAttrTypes, map[string]attr.Value{"type": types.StringValue(test.kind), "id": types.StringValue(test.id)})})
			(ruleReferencesValidator{subjects: test.subjects}).ValidateSet(t.Context(), validator.SetRequest{ConfigValue: value}, &response)
			if response.Diagnostics.HasError() == test.valid {
				t.Fatal(response.Diagnostics)
			}
		})
	}
	for _, value := range []types.Set{types.SetNull(types.ObjectType{AttrTypes: referenceAttrTypes}), types.SetUnknown(types.ObjectType{AttrTypes: referenceAttrTypes}), types.SetValueMust(types.ObjectType{AttrTypes: referenceAttrTypes}, []attr.Value{})} {
		var response validator.SetResponse
		(ruleReferencesValidator{}).ValidateSet(t.Context(), validator.SetRequest{ConfigValue: value}, &response)
		if response.Diagnostics.HasError() {
			t.Fatal(response.Diagnostics)
		}
	}
}
func TestCatalogDataSourceInventory(t *testing.T) {
	for _, empty := range []bool{true, false} {
		t.Run(map[bool]string{true: "empty", false: "sorted"}[empty], func(t *testing.T) {
			calls := 0
			conn := testManagementClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" {
					t.Error("lookup mutated API")
				}
				if strings.HasSuffix(r.URL.Path, "/entries") {
					if r.URL.Query().Get("all") != "true" {
						t.Error("inventory must bypass access-policy filtering for administrators")
					}
					if empty {
						_, _ = w.Write([]byte(`{"items":null}`))
					} else {
						_, _ = w.Write([]byte(`{"items":[{"id":"entry2","manifest":{"name":"Second"},"sourceURL":"https://example.com"},{"id":"entry1","manifest":{"name":"First"}}]}`))
					}
					return
				}
				_, _ = w.Write([]byte(`{"id":"default","sourceURLs":null,"sourceURLCredentials":{"url":"secret-token"}}`))
			})
			d := &mcpCatalogDataSource{client: conn}
			var schema datasource.SchemaResponse
			d.Schema(t.Context(), datasource.SchemaRequest{}, &schema)
			state := tfsdk.State{Schema: schema.Schema}
			model := catalogDataModel{ID: types.StringValue("default"), Sources: types.SetNull(types.StringType), GitCredentialIDs: types.MapNull(types.StringType), SyncErrors: types.MapNull(types.StringType), Entries: types.ListNull(types.ObjectType{AttrTypes: map[string]attr.Type{"id": types.StringType, "name": types.StringType, "source_url": types.StringType}})}
			if ds := state.Set(t.Context(), &model); ds.HasError() {
				t.Fatal(ds)
			}
			response := datasource.ReadResponse{State: state}
			d.Read(t.Context(), datasource.ReadRequest{Config: tfsdk.Config(state)}, &response)
			if response.Diagnostics.HasError() {
				t.Fatal(response.Diagnostics)
			}
			if ds := response.State.Get(t.Context(), &model); ds.HasError() {
				t.Fatal(ds)
			}
			if calls != 2 || model.Sources.IsNull() || model.Entries.IsNull() {
				t.Fatal("incorrect empty collection semantics")
			}
			if !empty {
				first, ok := model.Entries.Elements()[0].(types.Object)
				if !ok || !first.Attributes()["id"].Equal(types.StringValue("entry1")) {
					t.Fatal("inventory is not sorted")
				}
			}
		})
	}
}

func TestCatalogDataSourceStripsSourceCredentials(t *testing.T) {
	// Obot accepts source URLs with a Git token as userinfo and reports them
	// unchanged; none of the token may reach state.
	const withPassword = "https://x-access-token:ghp_password123@github.com/example/one"
	const withToken = "https://ghp_usertoken456@github.com/example/two"
	// Obot keeps a source apart from one differing only in credentials.
	const sameRepo = "https://ghp_othertoken789@github.com/example/two"
	const withQuery = "https://gitlab.example/raw?private_token=glpat-querytoken0&ref=main"
	conn := testManagementClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/entries") {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{
				{"id": "entry1", "manifest": map[string]string{"name": "One"}, "sourceURL": withPassword},
				{"id": "entry2", "manifest": map[string]string{"name": "Two"}, "sourceURL": withToken},
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "default", "sourceURLs": []string{withPassword, withToken, sameRepo, withQuery, "https://github.com/example/public"},
			"sourceURLGitCredentialIDs": map[string]string{withPassword: "gc1one", withToken: "gc1two", sameRepo: "gc1three"},
			"syncErrors": map[string]string{
				withToken:    "fatal: Authentication failed for '" + withToken + "'",
				withPassword: "clone of " + withPassword + " failed; credential ghp_password123 rejected",
				sameRepo:     "fatal: repository not found via " + sameRepo,
				withQuery:    "failed to read catalog " + withQuery + ": 401; token glpat-querytoken0 expired; ref main",
			},
		})
	})
	d := &mcpCatalogDataSource{client: conn}
	var schema datasource.SchemaResponse
	d.Schema(t.Context(), datasource.SchemaRequest{}, &schema)
	state := tfsdk.State{Schema: schema.Schema}
	model := catalogDataModel{ID: types.StringValue("default"), Sources: types.SetNull(types.StringType), GitCredentialIDs: types.MapNull(types.StringType), SyncErrors: types.MapNull(types.StringType), Entries: types.ListNull(types.ObjectType{AttrTypes: map[string]attr.Type{"id": types.StringType, "name": types.StringType, "source_url": types.StringType}})}
	if ds := state.Set(t.Context(), &model); ds.HasError() {
		t.Fatal(ds)
	}
	response := datasource.ReadResponse{State: state}
	d.Read(t.Context(), datasource.ReadRequest{Config: tfsdk.Config(state)}, &response)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	raw := response.State.Raw.String()
	for _, secret := range []string{"ghp_password123", "ghp_usertoken456", "ghp_othertoken789", "x-access-token", "glpat-querytoken0"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("state contains %q: %s", secret, raw)
		}
	}
	for _, want := range []string{"https://github.com/example/one", "https://github.com/example/public", "gc1one",
		"https://gitlab.example/raw?private_token=REDACTED&ref=REDACTED", "ref main"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("state lost %q: %s", want, raw)
		}
	}
	model = catalogDataModel{}
	if ds := response.State.Get(t.Context(), &model); ds.HasError() {
		t.Fatal(ds)
	}
	// The two sources for example/two share a key, keeping both values.
	ids := map[string]string{}
	errors := map[string]string{}
	model.GitCredentialIDs.ElementsAs(t.Context(), &ids, false)
	model.SyncErrors.ElementsAs(t.Context(), &errors, false)
	two := "https://github.com/example/two"
	if ids[two] != "gc1three, gc1two" || !strings.Contains(errors[two], "Authentication failed") ||
		!strings.Contains(errors[two], "repository not found") || len(model.Sources.Elements()) != 4 {
		t.Fatalf("colliding sources lost values: ids=%v errors=%q sources=%v", ids, errors[two], model.Sources)
	}
}

// Verify the framework schema of every new object, including nested collections.
func TestManagementSchemas(t *testing.T) {
	for _, r := range []resource.Resource{newMCPCatalogSourceResource(), newMCPAccessRuleResource()} {
		var response resource.SchemaResponse
		r.Schema(t.Context(), resource.SchemaRequest{}, &response)
		if d := response.Schema.ValidateImplementation(context.Background()); d.HasError() {
			t.Fatal(d)
		}
	}
	for _, d := range []datasource.DataSource{newMCPCatalogDataSource(), newCurrentAuthProviderDataSource(), newMCPAccessRuleDataSource(), newMCPAccessRulesDataSource()} {
		var response datasource.SchemaResponse
		d.Schema(t.Context(), datasource.SchemaRequest{}, &response)
		if ds := response.Schema.ValidateImplementation(context.Background()); ds.HasError() {
			t.Fatal(ds)
		}
	}
}

func TestMCPAccessRulesDataSourceList(t *testing.T) {
	for _, test := range []struct {
		name, body string
		count      int
	}{
		{name: "empty", body: `{"items":null}`, count: 0},
		{name: "sorted", body: `{"items":[{"id":"acr2","mcpCatalogID":"default","displayName":"Second"},{"id":"acr1","mcpCatalogID":"default","displayName":"First"}]}`, count: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn := testManagementClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("data source mutated API")
				}
				_, _ = w.Write([]byte(test.body))
			})
			d := &mcpAccessRulesDataSource{client: conn}
			var schema datasource.SchemaResponse
			d.Schema(t.Context(), datasource.SchemaRequest{}, &schema)
			state := tfsdk.State{Schema: schema.Schema}
			if diagnostics := state.Set(t.Context(), &accessRulesDataModel{CatalogID: types.StringNull(), Rules: types.ListNull(types.ObjectType{AttrTypes: accessRuleDataModel{}.attrTypes()})}); diagnostics.HasError() {
				t.Fatal(diagnostics)
			}
			response := datasource.ReadResponse{State: state}
			d.Read(t.Context(), datasource.ReadRequest{Config: tfsdk.Config(state)}, &response)
			if response.Diagnostics.HasError() {
				t.Fatal(response.Diagnostics)
			}
			var model accessRulesDataModel
			if diagnostics := response.State.Get(t.Context(), &model); diagnostics.HasError() {
				t.Fatal(diagnostics)
			}
			if model.Rules.IsNull() || len(model.Rules.Elements()) != test.count || model.CatalogID.ValueString() != "default" {
				t.Fatalf("incorrect list: %v", model)
			}
			if test.count > 0 {
				first, ok := model.Rules.Elements()[0].(types.Object)
				if !ok || !first.Attributes()["id"].Equal(types.StringValue("acr1")) {
					t.Fatal("policies not sorted")
				}
			}
		})
	}
}

func TestMCPAccessRuleDataSourceRead(t *testing.T) {
	for _, test := range []struct {
		name, body string
		wantError  bool
	}{
		{name: "found", body: `{"id":"acr1","mcpCatalogID":"default","displayName":"First","subjects":[{"type":"group","id":"okta/team"}],"resources":[]}`},
		{name: "scope mismatch", body: `{"id":"acr1","mcpCatalogID":"other"}`, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn := testManagementClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("data source mutated API")
				}
				_, _ = w.Write([]byte(test.body))
			})
			d := &mcpAccessRuleDataSource{client: conn}
			var schema datasource.SchemaResponse
			d.Schema(t.Context(), datasource.SchemaRequest{}, &schema)
			var diagnostics diag.Diagnostics
			config := newAccessRuleDataModel(t.Context(), client.AccessRule{ID: "acr1"}, &diagnostics)
			config.CatalogID = types.StringNull()
			config.Subjects = types.SetNull(types.ObjectType{AttrTypes: referenceAttrTypes})
			config.Resources = types.SetNull(types.ObjectType{AttrTypes: referenceAttrTypes})
			config.DisplayName, config.Generated = types.StringNull(), types.BoolNull()
			state := tfsdk.State{Schema: schema.Schema}
			diagnostics.Append(state.Set(t.Context(), &config)...)
			if diagnostics.HasError() {
				t.Fatal(diagnostics)
			}
			response := datasource.ReadResponse{State: state}
			d.Read(t.Context(), datasource.ReadRequest{Config: tfsdk.Config(state)}, &response)
			if response.Diagnostics.HasError() != test.wantError {
				t.Fatal(response.Diagnostics)
			}
			if test.wantError {
				return
			}
			var model accessRuleDataModel
			if diagnostics := response.State.Get(t.Context(), &model); diagnostics.HasError() {
				t.Fatal(diagnostics)
			}
			if len(model.Subjects.Elements()) != 1 || model.CatalogID.ValueString() != "default" {
				t.Fatalf("rule metadata missing: %v", model)
			}
		})
	}
}
