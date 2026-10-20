// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/identityschema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/obot-platform/terraform-provider-obot/internal/client"
)

var (
	_ resource.Resource                     = &mcpCatalogEntryResource{}
	_ resource.ResourceWithConfigure        = &mcpCatalogEntryResource{}
	_ resource.ResourceWithConfigValidators = &mcpCatalogEntryResource{}
	_ resource.ResourceWithImportState      = &mcpCatalogEntryResource{}
	_ resource.ResourceWithIdentity         = &mcpCatalogEntryResource{}
)

// importedValueVersion records a header whose value Obot stores but whose
// value_wo_version Terraform does not know, as after import. Configured
// versions start at 1, so any configuration differs from it and is applied.
const importedValueVersion = 0

// catalogEntryDeleteTimeout bounds the wait for Obot to finish removing an
// entry, which must complete before a tunnel it references can be deleted.
const catalogEntryDeleteTimeout = time.Minute

// staticRevisionKey is the private state key recording the revision of the
// header values Terraform last wrote or saw. Reading an entry never returns the values,
// so a new revision is the only sign they were changed outside Terraform.
const staticRevisionKey = "static_configuration_revision"

func staticRevision(entry *client.CatalogEntryDefinition) []byte {
	encoded, _ := json.Marshal(entry.Manifest.StaticConfigurationRevision)
	return encoded
}

type mcpCatalogEntryResource struct {
	client *client.Client
}

type catalogEntryResourceModel struct {
	ID               types.String              `tfsdk:"id"`
	CatalogID        types.String              `tfsdk:"catalog_id"`
	Name             types.String              `tfsdk:"name"`
	ShortDescription types.String              `tfsdk:"short_description"`
	Description      types.String              `tfsdk:"description"`
	Icon             types.String              `tfsdk:"icon"`
	RepoURL          types.String              `tfsdk:"repo_url"`
	Metadata         types.Map                 `tfsdk:"metadata"`
	Remote           *catalogEntryRemoteModel  `tfsdk:"remote"`
	Headers          []catalogEntryHeaderModel `tfsdk:"headers"`
}

type catalogEntryRemoteModel struct {
	FixedURL            types.String `tfsdk:"fixed_url"`
	Hostname            types.String `tfsdk:"hostname"`
	TunnelID            types.String `tfsdk:"tunnel_id"`
	StaticOAuthRequired types.Bool   `tfsdk:"static_oauth_required"`
}

type catalogEntryHeaderModel struct {
	Key            types.String `tfsdk:"key"`
	Name           types.String `tfsdk:"name"`
	Description    types.String `tfsdk:"description"`
	Required       types.Bool   `tfsdk:"required"`
	Sensitive      types.Bool   `tfsdk:"sensitive"`
	Prefix         types.String `tfsdk:"prefix"`
	ValueWO        types.String `tfsdk:"value_wo"`
	ValueWOVersion types.Int64  `tfsdk:"value_wo_version"`
}

type catalogEntryIdentityModel struct {
	ID types.String `tfsdk:"id"`
}

func newMCPCatalogEntryResource() resource.Resource {
	return &mcpCatalogEntryResource{}
}

func (r *mcpCatalogEntryResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mcp_catalog_entry"
}

func optionalString(description string) schema.StringAttribute {
	return schema.StringAttribute{
		Optional:            true,
		Computed:            true,
		Default:             stringdefault.StaticString(""),
		MarkdownDescription: description,
	}
}

func (r *mcpCatalogEntryResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	remoteURLField := func(description string, validators ...validator.String) schema.StringAttribute {
		return schema.StringAttribute{
			Optional:            true,
			MarkdownDescription: description,
			Validators:          append([]validator.String{trimmedString(false)}, validators...),
		}
	}
	shortDescription := optionalString("One-line summary shown in the catalog, at most 160 characters. Defaults to an empty string.")
	shortDescription.Validators = []validator.String{stringvalidator.UTF8LengthAtMost(160)}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a remote MCP server entry created directly in the default catalog, as the Obot UI's Add Server does. Requires an Obot Admin or Owner. " +
			"Use it for entries a catalog source cannot define, such as one reached through an `obot_mcp_tunnel`. " +
			"Entries synchronized from a Git source belong to `obot_mcp_catalog_source` and cannot be managed or imported here. " +
			"Header values are write-only: Obot stores them as credentials and does not return them when an entry is read, " +
			"although Obot administrators can reveal them in the Obot UI or API. " +
			"A value changed outside Terraform, as in the Obot UI, is detected on refresh, and the next apply sends the configured values again. " +
			"Obot replaces an entry's whole definition on update, so an entry with settings this resource does not represent, " +
			"such as a URL template, environment configuration, or a secret-bound header added in Obot, is never updated: refresh warns, " +
			"apply refuses to change it rather than erase those settings, and destroy still works. " +
			"Grant access to the entry with an `obot_mcp_access_rule` resource of type `mcpServerCatalogEntry`. " +
			"See the [tunnel guide](../guides/mcp-tunnels.md) for a tunnel, entry, and access policy together.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The Obot-generated catalog entry ID, used by access policies and import.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"catalog_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The catalog containing this entry, always `default`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Display name. Obot derives the entry ID from the name at creation; " +
					"renaming updates the entry in place and keeps its ID.",
				Validators: []validator.String{trimmedString(false)},
			},
			"short_description": shortDescription,
			"description":       optionalString("Markdown description shown on the entry's page. Defaults to an empty string."),
			"icon":              optionalString("Icon URL. Defaults to an empty string."),
			"repo_url":          optionalString("Source repository URL shown on the entry's page. Defaults to an empty string."),
			"metadata": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Catalog metadata, such as `categories`, a comma-separated list of categories. " +
					"Keys and values are strings.",
			},
			"remote": schema.SingleNestedAttribute{
				Required: true,
				MarkdownDescription: "How users reach the remote MCP server. Set exactly one of `fixed_url` or `hostname`. " +
					"Obot checks URLs not reached through a tunnel against its outbound network restrictions.",
				Attributes: map[string]schema.Attribute{
					"fixed_url": remoteURLField("HTTP or HTTPS URL every user connects to. "+
						"With `tunnel_id`, this is the URL as the tunnel agent's network sees it, and it cannot contain `${...}` references.",
						remoteFixedURL{}),
					"hostname": remoteURLField("Host that each user's own URL must be on, such as `mcp.example.com` or `*.example.com`. "+
						"Users enter their URL when connecting.",
						stringvalidator.RegexMatches(remoteHostnamePattern, "must be a host name, optionally starting with *.")),
					"tunnel_id": schema.StringAttribute{
						Optional: true,
						MarkdownDescription: "ID of an `obot_mcp_tunnel` through which Obot reaches the server. " +
							"The tunnel must exist, and its `allowed_urls` must allow `fixed_url` or `hostname`. " +
							"Obot then refuses to delete the tunnel, or to narrow its allowlist, while this entry uses it.",
						Validators: []validator.String{tunnelID()},
					},
					"static_oauth_required": schema.BoolAttribute{
						Optional:            true,
						Computed:            true,
						Default:             booldefault.StaticBool(false),
						MarkdownDescription: "Whether an administrator must configure OAuth client credentials for the server in Obot before users connect. Defaults to `false`.",
					},
				},
			},
			"headers": schema.ListNestedAttribute{
				Optional: true,
				MarkdownDescription: "HTTP headers sent to the server, in order. A header with `value_wo` sends that fixed value, which users never see. " +
					"A header without one is supplied by each user when connecting.",
				Validators: []validator.List{uniqueHeaderKeys{}},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"key": schema.StringAttribute{
							Required: true,
							MarkdownDescription: "Header name, in upper case with hyphens, such as `X-API-KEY`. " +
								"Obot stores header names in this form, so other spellings are rejected. Unique within the entry.",
							Validators: []validator.String{headerKey{}},
						},
						"name":        optionalString("Label shown to users. Defaults to an empty string."),
						"description": optionalString("Help text shown to users. Defaults to an empty string."),
						"required": schema.BoolAttribute{
							Optional:            true,
							Computed:            true,
							Default:             booldefault.StaticBool(false),
							MarkdownDescription: "Whether users must supply a value. Defaults to `false`.",
						},
						"sensitive": schema.BoolAttribute{
							Optional:            true,
							Computed:            true,
							Default:             booldefault.StaticBool(false),
							MarkdownDescription: "Whether Obot treats a user-supplied value as a secret. Defaults to `false`.",
						},
						"prefix": optionalString("Text Obot puts before a user-supplied value, such as `Bearer `. Defaults to an empty string."),
						"value_wo": schema.StringAttribute{
							Optional:  true,
							Sensitive: true,
							WriteOnly: true,
							MarkdownDescription: "Write-only fixed value, never stored in Terraform state. Requires Terraform 1.11 or later and `value_wo_version`. " +
								"Obot stores it as a credential and does not return it when the entry is read; Obot administrators can reveal it.",
							Validators: []validator.String{
								stringvalidator.LengthAtLeast(1),
								stringvalidator.AlsoRequires(path.MatchRelative().AtParent().AtName("value_wo_version")),
							},
						},
						"value_wo_version": schema.Int64Attribute{
							Optional: true,
							MarkdownDescription: "Version of `value_wo`, at least 1. Terraform cannot compare write-only values, so change this to send `value_wo` again; " +
								"otherwise Obot keeps the stored value. Remove both to make the header user-supplied. " +
								"Refresh records 0 when Obot holds a value Terraform did not send, such as after import, so the next apply replaces it.",
							Validators: []validator.Int64{
								int64validator.AtLeast(1),
								int64validator.AlsoRequires(path.MatchRelative().AtParent().AtName("value_wo")),
							},
						},
					},
				},
			},
		},
	}
}

func (r *mcpCatalogEntryResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	remote := path.MatchRoot("remote")
	return []resource.ConfigValidator{
		resourcevalidator.ExactlyOneOf(remote.AtName("fixed_url"), remote.AtName("hostname")),
		remoteEntryValidator{},
	}
}

func (r *mcpCatalogEntryResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = identityschema.Schema{
		Attributes: map[string]identityschema.Attribute{
			"id": identityschema.StringAttribute{
				RequiredForImport: true,
				Description:       "The Obot-generated catalog entry ID.",
			},
		},
	}
}

func (r *mcpCatalogEntryResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if conn := configuredClient(req.ProviderData, &resp.Diagnostics); conn != nil {
		r.client = conn
	}
}

func stringOrNull(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}

func findHeader(headers []catalogEntryHeaderModel, key string) *catalogEntryHeaderModel {
	for i := range headers {
		if headers[i].Key.ValueString() == key {
			return &headers[i]
		}
	}
	return nil
}

// fromAPI copies an entry into the model. Header value versions come from the
// prior state or plan, since Obot reports only whether a value is stored.
func (m *catalogEntryResourceModel) fromAPI(ctx context.Context, entry *client.CatalogEntryDefinition, diagnostics *diag.Diagnostics) {
	manifest := entry.Manifest
	m.ID = types.StringValue(entry.ID)
	m.CatalogID = types.StringValue(entry.CatalogName)
	m.Name = types.StringValue(manifest.Name)
	m.ShortDescription = types.StringValue(manifest.ShortDescription)
	m.Description = types.StringValue(manifest.Description)
	m.Icon = types.StringValue(manifest.Icon)
	m.RepoURL = types.StringValue(manifest.RepoURL)
	// Obot omits empty metadata, so an explicitly empty map is kept as configured.
	switch {
	case len(manifest.Metadata) > 0:
		value, d := types.MapValueFrom(ctx, types.StringType, manifest.Metadata)
		diagnostics.Append(d...)
		m.Metadata = value
	case m.Metadata.IsNull() || m.Metadata.IsUnknown() || len(m.Metadata.Elements()) > 0:
		m.Metadata = types.MapNull(types.StringType)
	}
	remote := client.RemoteCatalogConfig{}
	if manifest.RemoteConfig != nil {
		remote = *manifest.RemoteConfig
	}
	m.Remote = &catalogEntryRemoteModel{
		FixedURL:            stringOrNull(remote.FixedURL),
		Hostname:            stringOrNull(remote.Hostname),
		TunnelID:            stringOrNull(remote.TunnelName),
		StaticOAuthRequired: types.BoolValue(remote.StaticOAuthRequired),
	}
	prior := m.Headers
	headers := []catalogEntryHeaderModel{}
	for _, config := range manifest.Config {
		// Only headers are modeled. Dropping other configuration from state is
		// safe only because Update re-reads the entry and refuses to replace
		// one with unmodeled settings; Read warns about them.
		if config.Usage != client.HeaderUsage {
			continue
		}
		header := catalogEntryHeaderModel{
			Key:            types.StringValue(config.Key),
			Name:           types.StringValue(config.Name),
			Description:    types.StringValue(config.Description),
			Required:       types.BoolValue(config.Required),
			Sensitive:      types.BoolValue(config.Sensitive),
			Prefix:         types.StringValue(config.Prefix),
			ValueWO:        types.StringNull(),
			ValueWOVersion: types.Int64Null(),
		}
		if config.Static || config.Value != "" {
			header.ValueWOVersion = types.Int64Value(importedValueVersion)
			if previous := findHeader(prior, config.Key); previous != nil && !previous.ValueWOVersion.IsNull() && !previous.ValueWOVersion.IsUnknown() {
				header.ValueWOVersion = previous.ValueWOVersion
			}
		}
		headers = append(headers, header)
	}
	if len(headers) == 0 && prior == nil {
		headers = nil
	}
	m.Headers = headers
}

// manifest builds the definition to submit. A header whose version matches
// prior state is sent without a value, so Obot keeps the stored one; a new or
// changed version sends value_wo from the configuration.
func (m catalogEntryResourceModel) manifest(ctx context.Context, prior []catalogEntryHeaderModel, config tfsdk.Config, diagnostics *diag.Diagnostics) client.CatalogEntryManifest {
	result := client.CatalogEntryManifest{
		Name:             m.Name.ValueString(),
		ShortDescription: m.ShortDescription.ValueString(),
		Description:      m.Description.ValueString(),
		Icon:             m.Icon.ValueString(),
		RepoURL:          m.RepoURL.ValueString(),
		Runtime:          client.RemoteRuntime,
		Config:           []client.CatalogEntryConfig{},
	}
	if !m.Metadata.IsNull() {
		diagnostics.Append(m.Metadata.ElementsAs(ctx, &result.Metadata, false)...)
	}
	if m.Remote != nil {
		result.RemoteConfig = &client.RemoteCatalogConfig{
			FixedURL:            m.Remote.FixedURL.ValueString(),
			Hostname:            m.Remote.Hostname.ValueString(),
			TunnelName:          m.Remote.TunnelID.ValueString(),
			StaticOAuthRequired: m.Remote.StaticOAuthRequired.ValueBool(),
		}
	}
	var configured []catalogEntryHeaderModel
	if len(m.Headers) > 0 {
		diagnostics.Append(config.GetAttribute(ctx, path.Root("headers"), &configured)...)
		if diagnostics.HasError() {
			return result
		}
	}
	for i, header := range m.Headers {
		field := client.CatalogEntryConfig{
			Key:         header.Key.ValueString(),
			Name:        header.Name.ValueString(),
			Description: header.Description.ValueString(),
			Required:    header.Required.ValueBool(),
			Sensitive:   header.Sensitive.ValueBool(),
			Prefix:      header.Prefix.ValueString(),
			Usage:       client.HeaderUsage,
		}
		if !header.ValueWOVersion.IsNull() {
			if previous := findHeader(prior, field.Key); previous != nil && previous.ValueWOVersion.Equal(header.ValueWOVersion) {
				field.Static = true
			} else if i < len(configured) && configured[i].ValueWO.ValueString() != "" {
				field.Value = configured[i].ValueWO.ValueString()
			} else {
				diagnostics.AddAttributeError(path.Root("headers").AtListIndex(i).AtName("value_wo"), "Missing Header Value",
					"value_wo_version changed, but value_wo has no value to send. Write-only attributes require Terraform 1.11 or later.")
			}
		}
		result.Config = append(result.Config, field)
	}
	return result
}

func setCatalogEntryIdentity(ctx context.Context, identity *tfsdk.ResourceIdentity, id types.String, diagnostics *diag.Diagnostics) {
	if identity == nil {
		return
	}
	diagnostics.Append(identity.Set(ctx, catalogEntryIdentityModel{ID: id})...)
}

// checkManageable rejects entries this resource does not own: Git-synchronized
// entries, which Obot does not let the API change, and non-remote entries.
func checkManageable(entry *client.CatalogEntryDefinition, diagnostics *diag.Diagnostics) {
	switch {
	case entry.Detached:
		diagnostics.AddError("MCP Catalog Entry Is Detached From Its Source",
			fmt.Sprintf("Entry %s was synchronized from a catalog source that no longer provides it, so Obot does not allow changing it. "+
				"Accept ownership of it in Obot to make it editable, or remove it from Terraform state with terraform state rm.", entry.ID))
	case !entry.Editable:
		source := client.DisplaySourceURL(entry.SourceURL)
		diagnostics.AddError("MCP Catalog Entry Is Managed by a Catalog Source",
			fmt.Sprintf("Entry %s is synchronized from %s, and Obot does not allow changing it through the API. "+
				"Manage it by changing that source and its obot_mcp_catalog_source resource, and remove it from this resource with terraform state rm.", entry.ID, source))
	case entry.Manifest.Runtime != client.RemoteRuntime:
		diagnostics.AddError("Unsupported MCP Catalog Entry Runtime",
			fmt.Sprintf("Entry %s uses the %q runtime. obot_mcp_catalog_entry manages only remote entries.", entry.ID, entry.Manifest.Runtime))
	}
}

const unmodeledSummary = "MCP Catalog Entry Has Settings Terraform Cannot Manage"

// unmodeledDetail explains content the resource cannot represent. Obot
// replaces an entry's whole definition on update, so the resource reads and
// deletes such an entry but refuses to update it, which would erase them.
func unmodeledDetail(entry *client.CatalogEntryDefinition, consequence string) string {
	return fmt.Sprintf("Entry %s has settings obot_mcp_catalog_entry does not represent: %s. "+
		"Obot replaces an entry's whole definition on update, so %s "+
		"Remove them in Obot to manage the entry with Terraform, or remove it from Terraform state with terraform state rm and manage it in Obot.",
		entry.ID, strings.Join(entry.Unmodeled, ", "), consequence)
}

func (r *mcpCatalogEntryResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan catalogEntryResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	manifest := plan.manifest(ctx, nil, req.Config, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	entry, err := r.client.CreateCatalogEntry(ctx, defaultCatalogID, manifest)
	if err != nil {
		resp.Diagnostics.AddError("Error Creating MCP Catalog Entry", err.Error())
		return
	}
	plan.fromAPI(ctx, entry, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	setCatalogEntryIdentity(ctx, resp.Identity, plan.ID, &resp.Diagnostics)
	if resp.Private != nil {
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, staticRevisionKey, staticRevision(entry))...)
	}
}

func (r *mcpCatalogEntryResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state catalogEntryResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	entry, err := r.client.GetCatalogEntry(ctx, defaultCatalogID, state.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Error Reading MCP Catalog Entry", err.Error())
		return
	}
	checkManageable(entry, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if len(entry.Unmodeled) > 0 {
		// A warning rather than an error keeps plan and destroy working.
		resp.Diagnostics.AddWarning(unmodeledSummary, unmodeledDetail(entry, "Terraform will refuse to update it until they are removed; it can still be destroyed."))
	}
	state.fromAPI(ctx, entry, &resp.Diagnostics)
	recorded, d := req.Private.GetKey(ctx, staticRevisionKey)
	resp.Diagnostics.Append(d...)
	if current := staticRevision(entry); recorded != nil && !bytes.Equal(recorded, current) && state.forgetHeaderValues() {
		resp.Diagnostics.AddWarning("MCP Catalog Entry Header Values Changed Outside Terraform",
			fmt.Sprintf("The stored header values of entry %s were changed outside Terraform. "+
				"The next apply sends the configured value_wo of every header that has one.", entry.ID))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	setCatalogEntryIdentity(ctx, resp.Identity, state.ID, &resp.Diagnostics)
	if resp.Private != nil {
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, staticRevisionKey, staticRevision(entry))...)
	}
}

// forgetHeaderValues marks every stored header value as unknown to Terraform,
// as after import, so the next apply sends the configured values. It reports
// whether any header has a stored value.
func (m *catalogEntryResourceModel) forgetHeaderValues() bool {
	forgot := false
	for i := range m.Headers {
		if !m.Headers[i].ValueWOVersion.IsNull() {
			m.Headers[i].ValueWOVersion = types.Int64Value(importedValueVersion)
			forgot = true
		}
	}
	return forgot
}

func (r *mcpCatalogEntryResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state catalogEntryResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	recorded, d := req.Private.GetKey(ctx, staticRevisionKey)
	resp.Diagnostics.Append(d...)
	entry := r.update(ctx, plan, state, req.Config, recorded, &resp.Diagnostics)
	if entry == nil {
		return
	}
	plan.fromAPI(ctx, entry, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	setCatalogEntryIdentity(ctx, resp.Identity, plan.ID, &resp.Diagnostics)
	if resp.Private != nil {
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, staticRevisionKey, staticRevision(entry))...)
	}
}

// update replaces the entry after re-reading it, since settings or header
// values may have changed in Obot after the plan's refresh. recorded is the
// header value revision from private state. If the values changed since, a
// header sent as stored would keep the outside value and its new revision
// would hide the change, so every header with a version sends its configured
// value instead.
func (r *mcpCatalogEntryResource) update(ctx context.Context, plan, state catalogEntryResourceModel, config tfsdk.Config, recorded []byte, diagnostics *diag.Diagnostics) *client.CatalogEntryDefinition {
	current, err := r.client.GetCatalogEntry(ctx, defaultCatalogID, state.ID.ValueString())
	if err != nil {
		diagnostics.AddError("Error Updating MCP Catalog Entry", err.Error())
		return nil
	}
	if len(current.Unmodeled) > 0 {
		diagnostics.AddError(unmodeledSummary, unmodeledDetail(current, "applying would erase them, and the entry was not changed."))
		return nil
	}
	if recorded != nil && !bytes.Equal(recorded, staticRevision(current)) {
		state.Headers = append([]catalogEntryHeaderModel(nil), state.Headers...)
		state.forgetHeaderValues()
	}
	manifest := plan.manifest(ctx, state.Headers, config, diagnostics)
	if diagnostics.HasError() {
		return nil
	}
	entry, err := r.client.UpdateCatalogEntry(ctx, defaultCatalogID, state.ID.ValueString(), manifest)
	if err != nil {
		diagnostics.AddError("Error Updating MCP Catalog Entry", err.Error())
		return nil
	}
	return entry
}

// Delete waits for Obot to finish removing the entry, so that a tunnel it
// referenced can be deleted next in the same apply.
func (r *mcpCatalogEntryResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state catalogEntryResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	if err := r.client.DeleteCatalogEntry(ctx, defaultCatalogID, id); err != nil {
		resp.Diagnostics.AddError("Error Deleting MCP Catalog Entry", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(ctx, catalogEntryDeleteTimeout)
	defer cancel()
	for {
		_, err := r.client.GetCatalogEntry(ctx, defaultCatalogID, id)
		if client.IsNotFound(err) {
			return
		}
		if err != nil {
			resp.Diagnostics.AddError("Error Verifying MCP Catalog Entry Deletion", err.Error())
			return
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			resp.Diagnostics.AddError("Error Waiting for MCP Catalog Entry Deletion", ctx.Err().Error())
			return
		case <-timer.C:
		}
	}
}

// ImportState accepts the entry ID, optionally prefixed with "default/", or an
// identity. Reading an entry never returns header values, so refresh records stored ones
// with version 0 and the next apply sends the configured values.
func (r *mcpCatalogEntryResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id := req.ID
	if id == "" && resp.Identity != nil {
		var identity catalogEntryIdentityModel
		resp.Diagnostics.Append(resp.Identity.Get(ctx, &identity)...)
		id = identity.ID.ValueString()
	}
	id = strings.TrimPrefix(id, defaultCatalogID+"/")
	if !client.ValidObjectID(id) {
		resp.Diagnostics.AddError("Invalid MCP Catalog Entry Import ID",
			"Use the generated catalog entry ID, for example default-example-abc12, optionally as default/default-example-abc12.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("catalog_id"), defaultCatalogID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
	setCatalogEntryIdentity(ctx, resp.Identity, types.StringValue(id), &resp.Diagnostics)
	resp.Diagnostics.AddWarning("MCP Catalog Entry Header Values Not Imported",
		"Reading an entry never returns its header values. Headers with a stored value are imported with value_wo_version = 0, "+
			"so the next apply sends each configured value_wo, replacing the stored value, or makes the header user-supplied if none is configured.")
}
