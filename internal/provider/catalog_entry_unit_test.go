// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/obot-platform/terraform-provider-obot/internal/client"
)

const entriesPath = "/api/mcp-catalogs/default/entries"

// fakeCatalogEntries keeps catalog entries and their stored header values as
// Obot does: submitted values move into storage and are never returned.
type fakeCatalogEntries struct {
	mu      sync.Mutex
	t       *testing.T
	entries map[string]*client.CatalogEntryDefinition
	values  map[string]map[string]string
	// writes holds each create or update body.
	writes []client.CatalogEntryManifest
	// deleting counts reads that still return a deleted entry, as while
	// Obot's finalizer runs.
	deleting map[string]int
	nextID   int
}

func newFakeCatalogEntries(t *testing.T) (*fakeCatalogEntries, *client.Client) {
	t.Helper()
	fake := &fakeCatalogEntries{t: t, entries: map[string]*client.CatalogEntryDefinition{}, values: map[string]map[string]string{}, deleting: map[string]int{}}
	return fake, testManagementClient(t, fake.serve)
}

func (f *fakeCatalogEntries) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.URL.Path, entriesPath) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, entriesPath), "/")
	var manifest client.CatalogEntryManifest
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		if err := json.NewDecoder(r.Body).Decode(&manifest); err != nil {
			f.t.Error(err)
		}
		recorded := manifest
		recorded.Config = append([]client.CatalogEntryConfig(nil), manifest.Config...)
		f.writes = append(f.writes, recorded)
	}
	entry := f.entries[id]
	switch {
	case r.Method == http.MethodPost && id == "":
		f.nextID++
		id = fmt.Sprintf("default-%s-x%d", strings.ToLower(strings.ReplaceAll(manifest.Name, " ", "-")), f.nextID)
		entry = &client.CatalogEntryDefinition{ID: id, CatalogName: "default", Editable: true}
		if !f.store(w, entry, manifest, map[string]string{}) {
			return
		}
		f.entries[id] = entry
	case entry == nil:
		http.Error(w, "not found", http.StatusNotFound)
		return
	case r.Method == http.MethodGet:
		if remaining, ok := f.deleting[id]; ok {
			if remaining == 0 {
				delete(f.entries, id)
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			f.deleting[id] = remaining - 1
		}
	case !entry.Editable:
		http.Error(w, "entry is not editable", http.StatusBadRequest)
		return
	case r.Method == http.MethodPut:
		if !f.store(w, entry, manifest, f.values[id]) {
			return
		}
	case r.Method == http.MethodDelete:
		f.deleting[id] = 2
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_ = json.NewEncoder(w).Encode(entry)
}

// store applies Obot's static configuration rules to a submitted manifest.
func (f *fakeCatalogEntries) store(w http.ResponseWriter, entry *client.CatalogEntryDefinition, manifest client.CatalogEntryManifest, previous map[string]string) bool {
	values := map[string]string{}
	for i := range manifest.Config {
		field := &manifest.Config[i]
		field.Key = normalizeHeaderKey(field.Key)
		switch {
		case field.Value != "":
			values[field.Key] = field.Value
			field.Static = true
		case field.Static:
			if previous[field.Key] == "" {
				http.Error(w, fmt.Sprintf("static configuration %q requires a value", field.Key), http.StatusBadRequest)
				return false
			}
			values[field.Key] = previous[field.Key]
		}
		field.Value = ""
	}
	// Obot assigns a new revision only when the stored values change.
	manifest.StaticConfigurationRevision = entry.Manifest.StaticConfigurationRevision
	if len(values) == 0 {
		manifest.StaticConfigurationRevision = ""
	} else if !maps.Equal(values, previous) || manifest.StaticConfigurationRevision == "" {
		f.nextID++
		manifest.StaticConfigurationRevision = fmt.Sprintf("revision-%d", f.nextID)
	}
	entry.Manifest = manifest
	f.values[entry.ID] = values
	return true
}

func testEntryHeader(key string, version int64) catalogEntryHeaderModel {
	header := catalogEntryHeaderModel{
		Key: types.StringValue(key), Name: types.StringValue(""), Description: types.StringValue(""), Prefix: types.StringValue(""),
		Required: types.BoolValue(false), Sensitive: types.BoolValue(false), ValueWO: types.StringNull(), ValueWOVersion: types.Int64Null(),
	}
	if version != 0 {
		header.ValueWOVersion = types.Int64Value(version)
	}
	return header
}

func testEntryModel() catalogEntryResourceModel {
	return catalogEntryResourceModel{
		ID: types.StringUnknown(), CatalogID: types.StringUnknown(), Name: types.StringValue("Internal Search"),
		ShortDescription: types.StringValue("Search"), Description: types.StringValue(""), Icon: types.StringValue(""), RepoURL: types.StringValue(""),
		Metadata: types.MapValueMust(types.StringType, map[string]attr.Value{"categories": types.StringValue("Search")}),
		Remote: &catalogEntryRemoteModel{
			FixedURL: types.StringValue("https://search.internal.example/mcp"), Hostname: types.StringNull(),
			TunnelID: types.StringValue("mt1abc"), StaticOAuthRequired: types.BoolValue(false),
		},
		Headers: []catalogEntryHeaderModel{testEntryHeader("X-API-KEY", 1), testEntryHeader("X-USER-TOKEN", 0)},
	}
}

// testEntryRequest returns a plan and a configuration holding write-only
// values, which plans never contain.
func testEntryRequest(t *testing.T, r resource.Resource, plan catalogEntryResourceModel, values map[string]string) (tfsdk.Plan, tfsdk.Config) {
	t.Helper()
	config := plan
	config.Headers = append([]catalogEntryHeaderModel(nil), plan.Headers...)
	for i := range config.Headers {
		if value, ok := values[config.Headers[i].Key.ValueString()]; ok {
			config.Headers[i].ValueWO = types.StringValue(value)
		}
	}
	return tfsdk.Plan(testManagementState(t, r, plan)), tfsdk.Config(testManagementState(t, r, config))
}

func testEntryState(t *testing.T, state tfsdk.State) catalogEntryResourceModel {
	t.Helper()
	var model catalogEntryResourceModel
	if diagnostics := state.Get(t.Context(), &model); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	return model
}

func TestCatalogEntryLifecycleHeaderValues(t *testing.T) {
	fake, conn := newFakeCatalogEntries(t)
	r := &mcpCatalogEntryResource{client: conn}
	plan, config := testEntryRequest(t, r, testEntryModel(), map[string]string{"X-API-KEY": "first"})
	created := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema}}
	r.Create(t.Context(), resource.CreateRequest{Plan: plan, Config: config}, &created)
	if created.Diagnostics.HasError() {
		t.Fatal(created.Diagnostics)
	}
	sent := fake.writes[0]
	if sent.Runtime != "remote" || sent.RemoteConfig.TunnelName != "mt1abc" || sent.Metadata["categories"] != "Search" ||
		sent.Config[0].Value != "first" || sent.Config[1].Value != "" || sent.Config[1].Static || sent.Config[0].Usage != "header" {
		t.Fatalf("create body = %+v", sent)
	}
	model := testEntryState(t, created.State)
	id := model.ID.ValueString()
	if id == "" || model.CatalogID.ValueString() != "default" || !model.Headers[0].ValueWOVersion.Equal(types.Int64Value(1)) ||
		!model.Headers[0].ValueWO.IsNull() || !model.Headers[1].ValueWOVersion.IsNull() || model.Remote.Hostname.IsNull() == false {
		t.Fatalf("created state = %+v", model)
	}

	state := created.State
	update := func(change func(*catalogEntryResourceModel), values map[string]string) catalogEntryResourceModel {
		t.Helper()
		next := testEntryState(t, state)
		next.Headers = append([]catalogEntryHeaderModel(nil), next.Headers...)
		change(&next)
		plan, config := testEntryRequest(t, r, next, values)
		response := resource.UpdateResponse{State: state}
		r.Update(t.Context(), resource.UpdateRequest{Plan: plan, Config: config, State: state}, &response)
		if response.Diagnostics.HasError() {
			t.Fatal(response.Diagnostics)
		}
		state = response.State
		return testEntryState(t, state)
	}

	// Renaming keeps the ID and, sending no value, the stored value.
	model = update(func(m *catalogEntryResourceModel) { m.Name = types.StringValue("Renamed") }, map[string]string{"X-API-KEY": "ignored"})
	if last := fake.writes[len(fake.writes)-1]; last.Config[0].Value != "" || !last.Config[0].Static {
		t.Fatalf("unchanged value resent: %+v", last.Config[0])
	}
	if model.ID.ValueString() != id || fake.values[id]["X-API-KEY"] != "first" || model.Name.ValueString() != "Renamed" {
		t.Fatalf("rename lost the ID or value: %v %v", model, fake.values[id])
	}

	// A new version sends the configured value.
	model = update(func(m *catalogEntryResourceModel) { m.Headers[0].ValueWOVersion = types.Int64Value(2) }, map[string]string{"X-API-KEY": "second"})
	if fake.values[id]["X-API-KEY"] != "second" || model.Headers[0].ValueWOVersion.ValueInt64() != 2 {
		t.Fatalf("new value not sent: %v", fake.values[id])
	}

	// Renaming a header's key sends its value under the new key.
	model = update(func(m *catalogEntryResourceModel) { m.Headers[0].Key = types.StringValue("X-SEARCH-KEY") }, map[string]string{"X-SEARCH-KEY": "third"})
	if fake.values[id]["X-SEARCH-KEY"] != "third" {
		t.Fatalf("renamed header value not sent: %v", fake.values[id])
	}

	// Removing the value makes the header user-supplied.
	model = update(func(m *catalogEntryResourceModel) { m.Headers[0].ValueWOVersion = types.Int64Null() }, nil)
	if _, stored := fake.values[id]["X-SEARCH-KEY"]; stored || !model.Headers[0].ValueWOVersion.IsNull() {
		t.Fatalf("value not removed: %v", fake.values[id])
	}

	// Removing every header leaves an empty list, as configured.
	model = update(func(m *catalogEntryResourceModel) { m.Headers = []catalogEntryHeaderModel{} }, nil)
	if model.Headers == nil || len(model.Headers) != 0 || len(fake.entries[id].Manifest.Config) != 0 {
		t.Fatalf("headers = %v", model.Headers)
	}

	// Delete waits until Obot finishes removing the entry.
	response := resource.DeleteResponse{State: state}
	r.Delete(t.Context(), resource.DeleteRequest{State: state}, &response)
	if response.Diagnostics.HasError() || fake.entries[id] != nil {
		t.Fatal("delete did not wait for removal", response.Diagnostics)
	}
}

func TestCatalogEntryReadDetectsValueChanges(t *testing.T) {
	fake, conn := newFakeCatalogEntries(t)
	r := &mcpCatalogEntryResource{client: conn}
	plan, config := testEntryRequest(t, r, testEntryModel(), map[string]string{"X-API-KEY": "first"})
	created := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema}}
	r.Create(t.Context(), resource.CreateRequest{Plan: plan, Config: config}, &created)
	if created.Diagnostics.HasError() {
		t.Fatal(created.Diagnostics)
	}
	id := testEntryState(t, created.State).ID.ValueString()
	// Outside Terraform: the stored value is cleared and a value is set on the user-supplied header.
	fake.entries[id].Manifest.Config[0].Static = false
	fake.entries[id].Manifest.Config[1].Static = true
	fake.entries[id].Manifest.Metadata = nil
	response := resource.ReadResponse{State: created.State}
	r.Read(t.Context(), resource.ReadRequest{State: created.State}, &response)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	model := testEntryState(t, response.State)
	if !model.Headers[0].ValueWOVersion.IsNull() || !model.Headers[1].ValueWOVersion.Equal(types.Int64Value(importedValueVersion)) || !model.Metadata.IsNull() {
		t.Fatalf("value drift not detected: %+v", model.Headers)
	}
}

func TestCatalogEntryNotManageable(t *testing.T) {
	for name, entry := range map[string]client.CatalogEntryDefinition{
		"catalog source":          {SourceURL: "https://github.com/example/catalog", Manifest: client.CatalogEntryManifest{Runtime: "remote"}},
		"source with credentials": {SourceURL: "https://x-access-token:ghp_secret123@github.com/example/catalog", Manifest: client.CatalogEntryManifest{Runtime: "remote"}},
		"detached":                {Detached: true, Manifest: client.CatalogEntryManifest{Runtime: "remote"}},
		"not remote":              {Editable: true, Manifest: client.CatalogEntryManifest{Runtime: "npx"}},
	} {
		t.Run(name, func(t *testing.T) {
			mutations := 0
			conn := testManagementClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mutations++
				}
				entry.ID, entry.CatalogName = "default-synced-abc", "default"
				_ = json.NewEncoder(w).Encode(entry)
			})
			r := &mcpCatalogEntryResource{client: conn}
			model := testEntryModel()
			model.ID = types.StringValue("default-synced-abc")
			state := testManagementState(t, r, model)
			read := resource.ReadResponse{State: state}
			r.Read(t.Context(), resource.ReadRequest{State: state}, &read)
			if !read.Diagnostics.HasError() || mutations != 0 {
				t.Fatal("unmanageable entry was adopted")
			}
			detail := read.Diagnostics.Errors()[0].Detail()
			want := map[string]string{"catalog source": "obot_mcp_catalog_source", "source with credentials": "https://github.com/example/catalog", "detached": "Accept ownership", "not remote": "only remote"}[name]
			if strings.Contains(detail, "ghp_secret123") || strings.Contains(detail, "x-access-token") {
				t.Fatalf("error leaks source URL credentials: %s", detail)
			}
			if !strings.Contains(detail, want) {
				t.Fatalf("unhelpful error: %s", detail)
			}
		})
	}
}

func TestCatalogEntryUnmodeledSettings(t *testing.T) {
	// Settings added in Obot that the resource cannot represent are never
	// erased: refresh warns, update refuses, and delete still works.
	fake, conn := newFakeCatalogEntries(t)
	r := &mcpCatalogEntryResource{client: conn}
	plan, config := testEntryRequest(t, r, testEntryModel(), map[string]string{"X-API-KEY": "first"})
	created := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema}}
	r.Create(t.Context(), resource.CreateRequest{Plan: plan, Config: config}, &created)
	if created.Diagnostics.HasError() {
		t.Fatal(created.Diagnostics)
	}
	id := testEntryState(t, created.State).ID.ValueString()
	entry := fake.entries[id]
	entry.Manifest.Config = append(entry.Manifest.Config, client.CatalogEntryConfig{Key: "TENANT", Usage: "env"})

	read := resource.ReadResponse{State: created.State}
	r.Read(t.Context(), resource.ReadRequest{State: created.State}, &read)
	if read.Diagnostics.HasError() || len(read.Diagnostics.Warnings()) != 1 ||
		!strings.Contains(read.Diagnostics.Warnings()[0].Detail(), `config["TENANT"] with usage "env"`) {
		t.Fatalf("refresh did not warn: %v", read.Diagnostics)
	}

	writes := len(fake.writes)
	next := testEntryState(t, read.State)
	next.Name = types.StringValue("Renamed")
	plan, config = testEntryRequest(t, r, next, nil)
	updated := resource.UpdateResponse{State: read.State}
	r.Update(t.Context(), resource.UpdateRequest{Plan: plan, Config: config, State: read.State}, &updated)
	if !updated.Diagnostics.HasError() || len(fake.writes) != writes || entry.Manifest.Config[len(entry.Manifest.Config)-1].Key != "TENANT" {
		t.Fatalf("update erased unmodeled settings: %v", updated.Diagnostics)
	}

	deleted := resource.DeleteResponse{State: read.State}
	r.Delete(t.Context(), resource.DeleteRequest{State: read.State}, &deleted)
	if deleted.Diagnostics.HasError() || fake.entries[id] != nil {
		t.Fatalf("delete failed: %v", deleted.Diagnostics)
	}
}

func TestCatalogEntryUpdateAfterOutsideValueChange(t *testing.T) {
	// A value changed in Obot between the plan's refresh and the apply must
	// not survive an unrelated update, which would also hide its revision.
	for name, changed := range map[string]bool{"values changed": true, "values unchanged": false} {
		t.Run(name, func(t *testing.T) {
			fake, conn := newFakeCatalogEntries(t)
			r := &mcpCatalogEntryResource{client: conn}
			plan, config := testEntryRequest(t, r, testEntryModel(), map[string]string{"X-API-KEY": "first"})
			created := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema}}
			r.Create(t.Context(), resource.CreateRequest{Plan: plan, Config: config}, &created)
			if created.Diagnostics.HasError() {
				t.Fatal(created.Diagnostics)
			}
			state := testEntryState(t, created.State)
			id := state.ID.ValueString()
			recorded := staticRevision(fake.entries[id])
			if changed {
				fake.values[id]["X-API-KEY"] = "outside"
				fake.entries[id].Manifest.StaticConfigurationRevision = "revision-outside"
			}

			next := testEntryState(t, created.State)
			next.Description = types.StringValue("changed")
			_, config = testEntryRequest(t, r, next, map[string]string{"X-API-KEY": "first"})
			var diagnostics diag.Diagnostics
			if r.update(t.Context(), next, state, config, recorded, &diagnostics) == nil {
				t.Fatal(diagnostics)
			}
			// Either way Obot ends up with the configured value; only after an
			// outside change is it sent rather than kept.
			sent := fake.writes[len(fake.writes)-1].Config[0]
			if fake.values[id]["X-API-KEY"] != "first" || sent.Static == changed {
				t.Fatalf("stored %q, sent %+v", fake.values[id]["X-API-KEY"], sent)
			}
			if !state.Headers[0].ValueWOVersion.Equal(types.Int64Value(1)) {
				t.Fatal("update changed the prior state's headers")
			}
		})
	}
}

func TestCatalogEntryReadAndDeleteErrors(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			conn := testManagementClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
			r := &mcpCatalogEntryResource{client: conn}
			model := testEntryModel()
			model.ID = types.StringValue("default-entry-abc")
			state := testManagementState(t, r, model)
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
			if status != 404 && !strings.HasPrefix(read.Diagnostics.Errors()[0].Summary(), "Error Reading MCP Catalog Entry") {
				t.Fatal(read.Diagnostics)
			}
		})
	}
}

func TestCatalogEntryClientRejectsUnexpectedResponses(t *testing.T) {
	for name, body := range map[string]string{
		"invalid ID":    `{"id":"../x","catalogName":"default"}`,
		"other catalog": `{"id":"entry1","catalogName":"other"}`,
		"other entry":   `{"id":"entry2","catalogName":"default"}`,
	} {
		t.Run(name, func(t *testing.T) {
			conn := testManagementClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
			if _, err := conn.GetCatalogEntry(t.Context(), "default", "entry1"); err == nil {
				t.Fatal("unexpected response accepted")
			}
			if _, err := conn.UpdateCatalogEntry(t.Context(), "default", "entry1", client.CatalogEntryManifest{}); err == nil {
				t.Fatal("unexpected response accepted")
			}
		})
	}
	conn := testManagementClient(t, func(w http.ResponseWriter, _ *http.Request) { t.Error("invalid ID reached the API") })
	if _, err := conn.GetCatalogEntry(t.Context(), "default", "a/b"); err == nil {
		t.Fatal("invalid ID accepted")
	}
	if _, err := conn.UpdateCatalogEntry(t.Context(), "default", "", client.CatalogEntryManifest{}); err == nil {
		t.Fatal("empty ID accepted")
	}
}

func TestCatalogEntryImport(t *testing.T) {
	r := &mcpCatalogEntryResource{}
	for id, valid := range map[string]bool{
		"default-entry-abc": true, "default/default-entry-abc": true, "other/default-entry-abc": false, "default/": false, "": false, "a?b": false,
	} {
		state := testManagementState(t, r, testEntryModel())
		response := resource.ImportStateResponse{State: state}
		r.ImportState(t.Context(), resource.ImportStateRequest{ID: id}, &response)
		if response.Diagnostics.HasError() == valid {
			t.Errorf("import %q: %v", id, response.Diagnostics)
		}
		if valid && len(response.Diagnostics.Warnings()) != 1 {
			t.Errorf("import %q did not warn about header values", id)
		}
	}

	// Refreshing an imported entry records stored values with version 0.
	fake, conn := newFakeCatalogEntries(t)
	fake.entries["default-entry-abc"] = &client.CatalogEntryDefinition{ID: "default-entry-abc", CatalogName: "default", Editable: true, Manifest: client.CatalogEntryManifest{
		Name: "Imported", Runtime: "remote", RemoteConfig: &client.RemoteCatalogConfig{Hostname: "*.example.com"},
		Config: []client.CatalogEntryConfig{{Key: "X-API-KEY", Static: true, Usage: "header"}, {Key: "X-USER", Usage: "header", Required: true}},
	}}
	r.client = conn
	var schema resource.SchemaResponse
	r.Schema(t.Context(), resource.SchemaRequest{}, &schema)
	imported := resource.ImportStateResponse{State: tfsdk.State{Schema: schema.Schema}}
	imported.State.RemoveResource(t.Context())
	r.ImportState(t.Context(), resource.ImportStateRequest{ID: "default-entry-abc"}, &imported)
	read := resource.ReadResponse{State: imported.State}
	r.Read(t.Context(), resource.ReadRequest{State: imported.State}, &read)
	if read.Diagnostics.HasError() {
		t.Fatal(read.Diagnostics)
	}
	model := testEntryState(t, read.State)
	if len(model.Headers) != 2 || !model.Headers[0].ValueWOVersion.Equal(types.Int64Value(0)) || !model.Headers[1].ValueWOVersion.IsNull() ||
		model.Remote.Hostname.ValueString() != "*.example.com" || !model.Remote.FixedURL.IsNull() || model.Name.ValueString() != "Imported" {
		t.Fatalf("imported state = %+v %+v", model, model.Headers)
	}
}

func TestCatalogEntryMissingWriteOnlyValue(t *testing.T) {
	// A version without a write-only value, as from Terraform before 1.11, must not clear the header's value.
	_, conn := newFakeCatalogEntries(t)
	r := &mcpCatalogEntryResource{client: conn}
	model := testEntryModel()
	plan, config := testEntryRequest(t, r, model, nil)
	created := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema}}
	r.Create(t.Context(), resource.CreateRequest{Plan: plan, Config: config}, &created)
	if !created.Diagnostics.HasError() || created.Diagnostics.Errors()[0].Summary() != "Missing Header Value" {
		t.Fatalf("expected missing value error, got %v", created.Diagnostics)
	}
}

func TestCatalogEntryValidators(t *testing.T) {
	for key, valid := range map[string]bool{"X-API-KEY": true, "AUTHORIZATION": true, "x-api-key": false, "X_API_KEY": false, "": false, "X API": false} {
		var response validator.StringResponse
		headerKey{}.ValidateString(t.Context(), validator.StringRequest{ConfigValue: types.StringValue(key)}, &response)
		if response.Diagnostics.HasError() == valid {
			t.Errorf("key %q: %v", key, response.Diagnostics)
		}
	}
	var suggestion validator.StringResponse
	headerKey{}.ValidateString(t.Context(), validator.StringRequest{ConfigValue: types.StringValue("x_api_key")}, &suggestion)
	if !strings.Contains(suggestion.Diagnostics.Errors()[0].Detail(), `"X-API-KEY"`) {
		t.Error("no suggested key")
	}
	for raw, valid := range map[string]bool{
		"https://mcp.example.com/mcp": true, "http://10.0.0.5:8080/mcp": true, "https://${HOST}/mcp": true,
		"ftp://example.com": false, "https://user:pw@example.com": false, "mcp.example.com": false, "https:///mcp": false,
	} {
		var response validator.StringResponse
		remoteFixedURL{}.ValidateString(t.Context(), validator.StringRequest{ConfigValue: types.StringValue(raw)}, &response)
		if response.Diagnostics.HasError() == valid {
			t.Errorf("URL %q: %v", raw, response.Diagnostics)
		}
	}
	for host, valid := range map[string]bool{"mcp.example.com": true, "*.example.com": true, "localhost": true, "https://example.com": false, "a.*.com": false} {
		if remoteHostnamePattern.MatchString(host) != valid {
			t.Errorf("hostname %q", host)
		}
	}

	r := &mcpCatalogEntryResource{}
	validate := func(change func(*catalogEntryResourceModel)) bool {
		model := testEntryModel()
		change(&model)
		config := tfsdk.Config(testManagementState(t, r, model))
		var response resource.ValidateConfigResponse
		for _, v := range r.ConfigValidators(t.Context()) {
			// Some validators replace rather than append to the diagnostics.
			var each resource.ValidateConfigResponse
			v.ValidateResource(t.Context(), resource.ValidateConfigRequest{Config: config}, &each)
			response.Diagnostics.Append(each.Diagnostics...)
		}
		var schema resource.SchemaResponse
		r.Schema(t.Context(), resource.SchemaRequest{}, &schema)
		headers, _ := schema.Schema.Attributes["headers"].(interface {
			ListValidators() []validator.List
		})
		var list types.List
		response.Diagnostics.Append(config.GetAttribute(t.Context(), path.Root("headers"), &list)...)
		for _, v := range headers.ListValidators() {
			var lr validator.ListResponse
			v.ValidateList(t.Context(), validator.ListRequest{Path: path.Root("headers"), ConfigValue: list}, &lr)
			response.Diagnostics.Append(lr.Diagnostics...)
		}
		return !response.Diagnostics.HasError()
	}
	for name, test := range map[string]struct {
		change func(*catalogEntryResourceModel)
		valid  bool
	}{
		"tunneled fixed URL": {func(*catalogEntryResourceModel) {}, true},
		"hostname": {func(m *catalogEntryResourceModel) {
			m.Remote.FixedURL, m.Remote.Hostname = types.StringNull(), types.StringValue("*.example.com")
		}, true},
		"no URL":   {func(m *catalogEntryResourceModel) { m.Remote.FixedURL = types.StringNull() }, false},
		"two URLs": {func(m *catalogEntryResourceModel) { m.Remote.Hostname = types.StringValue("example.com") }, false},
		"untunneled template": {func(m *catalogEntryResourceModel) {
			m.Remote.TunnelID = types.StringNull()
			m.Remote.FixedURL = types.StringValue("https://${HOST}/mcp")
		}, true},
		"tunneled reference":   {func(m *catalogEntryResourceModel) { m.Remote.FixedURL = types.StringValue("https://${HOST}/mcp") }, false},
		"unknown tunnel":       {func(m *catalogEntryResourceModel) { m.Remote.TunnelID = types.StringUnknown() }, true},
		"duplicate header key": {func(m *catalogEntryResourceModel) { m.Headers[1].Key = types.StringValue("X-API-KEY") }, false},
	} {
		if validate(test.change) != test.valid {
			t.Errorf("%s: valid = %v", name, !test.valid)
		}
	}
}

func TestCatalogEntrySchema(t *testing.T) {
	var response resource.SchemaResponse
	newMCPCatalogEntryResource().Schema(t.Context(), resource.SchemaRequest{}, &response)
	if d := response.Schema.ValidateImplementation(context.Background()); d.HasError() {
		t.Fatal(d)
	}
	var identity resource.IdentitySchemaResponse
	(&mcpCatalogEntryResource{}).IdentitySchema(t.Context(), resource.IdentitySchemaRequest{}, &identity)
	if d := identity.IdentitySchema.ValidateImplementation(context.Background()); d.HasError() {
		t.Fatal(d)
	}
}
