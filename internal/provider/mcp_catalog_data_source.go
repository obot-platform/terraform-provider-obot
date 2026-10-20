// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/obot-platform/terraform-provider-obot/internal/client"
)

var (
	_ datasource.DataSource              = &mcpCatalogDataSource{}
	_ datasource.DataSourceWithConfigure = &mcpCatalogDataSource{}
)

type mcpCatalogDataSource struct {
	client *client.Client
}

type catalogDataModel struct {
	ID               types.String `tfsdk:"id"`
	DisplayName      types.String `tfsdk:"display_name"`
	Sources          types.Set    `tfsdk:"sources"`
	GitCredentialIDs types.Map    `tfsdk:"source_git_credential_ids"`
	LastSynced       types.String `tfsdk:"last_synced"`
	IsSyncing        types.Bool   `tfsdk:"is_syncing"`
	SyncErrors       types.Map    `tfsdk:"sync_errors"`
	Entries          types.List   `tfsdk:"entries"`
}

type catalogEntryModel struct {
	ID        types.String `tfsdk:"id"`
	Name      types.String `tfsdk:"name"`
	SourceURL types.String `tfsdk:"source_url"`
}

func (catalogEntryModel) attrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"id":         types.StringType,
		"name":       types.StringType,
		"source_url": types.StringType,
	}
}

func newMCPCatalogDataSource() datasource.DataSource {
	return &mcpCatalogDataSource{}
}

func (d *mcpCatalogDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mcp_catalog"
}

func (d *mcpCatalogDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads an MCP catalog's sources, synchronization status, and entry inventory. Use to discover configuration before import. " +
			"Reading never refreshes or changes the catalog. Git-owned entry definitions remain in Git; inline catalog credentials and server configurations are not exposed. " +
			"A source URL added in Obot with embedded credentials, such as `https://<token>@github.com/...`, appears with its userinfo removed and its query values replaced by `REDACTED`, since either can carry a working credential, and they are redacted from sync errors; use `obot_git_credential` for private sources instead. " +
			"A URL changed this way, including one with a harmless query such as `?ref=main`, is not the source's real URL: it cannot be used to import an `obot_mcp_catalog_source` or to look up that source's `url` in these maps. " +
			"Use an Admin, Owner, or Auditor token: Obot limits other tokens to the entries their access policies allow.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Catalog identifier, usually `default`.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(client.ObjectIDPattern, "must be an Obot catalog ID"),
				},
			},
			"display_name": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The catalog's display name.",
			},
			"sources": schema.SetAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Current catalog source URLs, with userinfo removed and query values redacted. Sources that differ only in credentials appear once.",
			},
			"source_git_credential_ids": schema.MapAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Existing Git credential IDs keyed by source URL, with userinfo removed and query values redacted; contains no token values. " +
					"Sources that differ only in credentials share a key, and their distinct IDs are joined with commas.",
			},
			"last_synced": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Timestamp of the last completed synchronization, or an empty string if none has completed.",
			},
			"is_syncing": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether synchronization is running or queued.",
			},
			"sync_errors": schema.MapAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Last synchronization errors keyed by source URL, with userinfo removed and query values redacted from keys, and those credentials redacted from messages. " +
					"Sources that differ only in credentials share a key, and their distinct messages are joined with newlines.",
			},
			"entries": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "Current catalog entries sorted by ID. Source-managed entries have a nonempty source URL; API-created entries have an empty one. " +
					"This is the current inventory and does not trigger synchronization.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "The catalog entry identifier.",
						},
						"name": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "The catalog entry's name.",
						},
						"source_url": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "The owning source URL, with userinfo removed and query values redacted, empty for API-created entries.",
						},
					},
				},
			},
		},
	}
}

func (d *mcpCatalogDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if conn := configuredClient(req.ProviderData, &resp.Diagnostics); conn != nil {
		d.client = conn
	}
}

// stringMapValue converts an API map, where nil means empty.
func stringMapValue(ctx context.Context, values map[string]string, diagnostics *diag.Diagnostics) types.Map {
	if values == nil {
		values = map[string]string{}
	}
	result, d := types.MapValueFrom(ctx, types.StringType, values)
	diagnostics.Append(d...)
	return result
}

// minRedactedSecret is the shortest credential redacted on its own from
// messages; whole URLs are always replaced.
const minRedactedSecret = 8

// sourceURLScrubber removes credentials from source URLs before they reach
// state. Obot accepts source URLs with a Git token as userinfo, although
// obot_mcp_catalog_source refuses them, and reports them unchanged.
type sourceURLScrubber struct {
	client  *client.Client
	secrets []string
	// stripped maps each raw URL seen to its stripped form.
	stripped map[string]string
}

func newSourceURLScrubber(conn *client.Client) *sourceURLScrubber {
	return &sourceURLScrubber{client: conn, stripped: map[string]string{}}
}

func (s *sourceURLScrubber) strip(raw string) string {
	stripped, secrets := client.StripURLCredentials(raw)
	if stripped != raw {
		s.stripped[raw] = stripped
	}
	for _, secret := range secrets {
		if secret != "" && !slices.Contains(s.secrets, secret) {
			s.secrets = append(s.secrets, secret)
		}
	}
	return stripped
}

// stripKeys strips credentials from map keys. With messages, values are
// server-reported text that can repeat a URL, so they are redacted of every
// credential seen once all keys are stripped. Obot keeps sources that differ
// only in credentials apart, so their stripped keys can collide; distinct
// values for one key are then joined in sorted order, newline-separated for
// messages and comma-separated otherwise, rather than one being dropped.
func (s *sourceURLScrubber) stripKeys(values map[string]string, messages bool) map[string]string {
	keys := slices.Sorted(maps.Keys(values))
	stripped := make([]string, len(keys))
	for i, key := range keys {
		stripped[i] = s.strip(key)
	}
	grouped := map[string][]string{}
	for i, key := range keys {
		value := values[key]
		if messages {
			value = s.redact(value)
		}
		if !slices.Contains(grouped[stripped[i]], value) {
			grouped[stripped[i]] = append(grouped[stripped[i]], value)
		}
	}
	separator := ", "
	if messages {
		separator = "\n"
	}
	result := make(map[string]string, len(grouped))
	for key, group := range grouped {
		slices.Sort(group)
		result[key] = strings.Join(group, separator)
	}
	return result
}

func (s *sourceURLScrubber) redact(message string) string {
	// Obot's messages usually repeat the whole URL, so replace it first,
	// longest first in case one URL contains another.
	raws := slices.Collect(maps.Keys(s.stripped))
	sort.Slice(raws, func(i, j int) bool { return len(raws[i]) > len(raws[j]) })
	for _, raw := range raws {
		message = strings.ReplaceAll(message, raw, s.stripped[raw])
	}
	// Then any credential repeated on its own, longest first, so a password
	// is not redacted before the userinfo containing it. Values shorter than
	// minRedactedSecret, such as a username or ref=main, would garble any
	// message containing the same word; a credential that short is not
	// protected by redaction anyway.
	secrets := slices.DeleteFunc(slices.Clone(s.secrets), func(secret string) bool { return len(secret) < minRedactedSecret })
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		message = strings.ReplaceAll(message, secret, "[REDACTED]")
	}
	if s.client != nil {
		message = s.client.Redact(message)
	}
	return message
}

func (d *mcpCatalogDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var model catalogDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	result, err := d.client.GetCatalog(ctx, model.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error Reading MCP Catalog", err.Error())
		return
	}
	entries, err := d.client.ListCatalogEntries(ctx, model.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error Reading MCP Catalog Entries", err.Error())
		return
	}
	urls := newSourceURLScrubber(d.client)
	sources := make([]string, 0, len(result.Sources))
	for _, source := range result.Sources {
		// Sources that differ only in credentials strip to one URL, and a set
		// cannot hold it twice.
		if stripped := urls.strip(source); !slices.Contains(sources, stripped) {
			sources = append(sources, stripped)
		}
	}
	model.DisplayName = types.StringValue(result.DisplayName)
	model.Sources = stringSetValue(ctx, sources, &resp.Diagnostics)
	model.GitCredentialIDs = stringMapValue(ctx, urls.stripKeys(result.GitCredentialIDs, false), &resp.Diagnostics)
	model.LastSynced = types.StringValue(result.LastSynced)
	model.IsSyncing = types.BoolValue(result.IsSyncing)
	model.SyncErrors = stringMapValue(ctx, urls.stripKeys(result.SyncErrors, true), &resp.Diagnostics)
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	items := make([]catalogEntryModel, 0, len(entries))
	for _, entry := range entries {
		items = append(items, catalogEntryModel{ID: types.StringValue(entry.ID), Name: types.StringValue(entry.Manifest.Name), SourceURL: types.StringValue(urls.strip(entry.SourceURL))})
	}
	var diagnostics diag.Diagnostics
	model.Entries, diagnostics = types.ListValueFrom(ctx, types.ObjectType{AttrTypes: catalogEntryModel{}.attrTypes()}, items)
	resp.Diagnostics.Append(diagnostics...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}
