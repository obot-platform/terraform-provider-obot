// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/identityschema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/obot-platform/terraform-provider-obot/internal/client"
)

var (
	_ resource.Resource                = &mcpTunnelResource{}
	_ resource.ResourceWithConfigure   = &mcpTunnelResource{}
	_ resource.ResourceWithIdentity    = &mcpTunnelResource{}
	_ resource.ResourceWithImportState = &mcpTunnelResource{}
	_ resource.ResourceWithModifyPlan  = &mcpTunnelResource{}
)

type mcpTunnelResource struct {
	client *client.Client
}

type tunnelResourceModel struct {
	ID                   types.String `tfsdk:"id"`
	DisplayName          types.String `tfsdk:"display_name"`
	Description          types.String `tfsdk:"description"`
	AllowedURLs          types.Set    `tfsdk:"allowed_urls"`
	Token                types.String `tfsdk:"token"`
	TokenPreview         types.String `tfsdk:"token_preview"`
	TokenRotationVersion types.Int64  `tfsdk:"token_rotation_version"`
	Command              types.String `tfsdk:"command"`
	DockerCommand        types.String `tfsdk:"docker_command"`
}

type tunnelIdentityModel struct {
	ID types.String `tfsdk:"id"`
}

func newMCPTunnelResource() resource.Resource {
	return &mcpTunnelResource{}
}

func (r *mcpTunnelResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mcp_tunnel"
}

func (r *mcpTunnelResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an MCP tunnel definition. Requires an Obot Admin or Owner. The tunnel can be created before an agent connects. " +
			"The full token is captured only on creation or explicit rotation, is sensitive, and is stored in Terraform state. " +
			"`command` and `docker_command` are the ready-to-run agent commands the Obot UI shows; they contain the token and follow its lifecycle. " +
			"Import adopts the definition without changing credentials and leaves `token` null. " +
			"Match existing metadata and keep `token_rotation_version` at 0 for adoption. " +
			"See the [tunnel guide](../guides/mcp-tunnels.md) for import, secret-manager publication, rotation, and recovery.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The generated Obot tunnel ID, used by remote MCP catalog entries and import.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"display_name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Human-readable tunnel name. Updating it preserves the tunnel ID and token.",
				Validators: []validator.String{
					trimmedString(false),
				},
			},
			"description": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(""),
				MarkdownDescription: "Description of the private network or purpose. Defaults to an empty string.",
				Validators: []validator.String{
					trimmedString(true),
				},
			},
			"allowed_urls": schema.SetAttribute{
				Required:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Permitted target URLs or hostnames. A rule may have one wildcard at its beginning or end. " +
					"An empty set denies all destinations. Obot rejects changes that would invalidate existing catalog-entry references.",
				Validators: []validator.Set{
					setvalidator.ValueStringsAre(trimmedString(false), allowedURLRule{}),
				},
			},
			"token": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "Complete tunnel bearer token, available after creation or explicit rotation. " +
					"Null after import or an external rotation. Never populated from a preview. " +
					"Protect Terraform state; sensitive values are still stored there.",
			},
			"command": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "Command that connects an agent with the Obot CLI, as shown by the Obot UI: " +
					"`obot tunnel --obot-base-url <API URL> --token <token>`. The URL is the provider's `base_url` normalized to end in `/api`. " +
					"Contains the complete `token`, so it is sensitive and stored in state. " +
					"Set with `token` on creation or rotation, rebuilt on refresh when `base_url` changes, and null whenever `token` is null.",
			},
			"docker_command": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "Command that connects an agent using the Obot container image, as shown by the Obot UI: " +
					"`docker run --rm ghcr.io/obot-platform/obot:<tag> tunnel --obot-base-url <API URL> --token <token>`. " +
					"The tag is the Obot server version without build metadata, or `main` for development builds or when the version cannot be read. " +
					"It is recorded when the token is captured, so upgrading Obot does not change it until the next rotation, or until a refresh after `base_url` changes rebuilds both commands. " +
					"Contains the complete `token`, so it is sensitive and stored in state. Null whenever `token` is null.",
			},
			"token_preview": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Non-secret shortened preview of the current token. It cannot authenticate an agent.",
			},
			"token_rotation_version": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(0),
				MarkdownDescription: "Terraform-local rotation trigger. Defaults to 0 and is initialized to 0 on import. " +
					"Increase it to explicitly rotate the secret without replacing the tunnel. Decreases are rejected. " +
					"Rotation immediately revokes the old token and disconnects its clients.",
				Validators: []validator.Int64{
					int64validator.AtLeast(0),
				},
			},
		},
	}
}

func (r *mcpTunnelResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = identityschema.Schema{
		Attributes: map[string]identityschema.Attribute{
			"id": identityschema.StringAttribute{
				RequiredForImport: true,
				Description:       "The generated Obot tunnel ID.",
			},
		},
	}
}

func (r *mcpTunnelResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	conn, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected Resource Configure Type", fmt.Sprintf("Expected an Obot API client, got %T.", req.ProviderData))
		return
	}
	r.client = conn
}

func (m tunnelResourceModel) manifest(ctx context.Context, diagnostics *diag.Diagnostics) client.TunnelManifest {
	urls := []string{}
	diagnostics.Append(m.AllowedURLs.ElementsAs(ctx, &urls, false)...)
	sort.Strings(urls)
	return client.TunnelManifest{
		DisplayName: m.DisplayName.ValueString(),
		Description: m.Description.ValueString(),
		AllowedURLs: urls,
	}
}

// setMetadata copies the server's definition into the model.
func (m *tunnelResourceModel) setMetadata(ctx context.Context, tunnel *client.Tunnel, diagnostics *diag.Diagnostics) {
	m.ID = types.StringValue(tunnel.ID)
	m.DisplayName = types.StringValue(tunnel.Manifest.DisplayName)
	m.Description = types.StringValue(tunnel.Manifest.Description)
	m.AllowedURLs = stringSetValue(ctx, tunnel.Manifest.AllowedURLs, diagnostics)
}

// captureToken stores a complete token returned by create or rotation.
func (m *tunnelResourceModel) captureToken(tunnel *client.Tunnel) {
	preview, err := client.TunnelTokenPreview(tunnel.Token)
	if err != nil {
		m.Token = types.StringNull()
		m.TokenPreview = types.StringNull()
		return
	}
	m.Token = types.StringValue(tunnel.Token)
	m.TokenPreview = types.StringValue(preview)
}

// tokenCommandsStale reports whether command values must be derived again:
// they are always cleared with the token, filled in when missing, and
// rebuilt when the provider's base URL no longer matches the one in them.
func (m tunnelResourceModel) tokenCommandsStale(baseURL string) bool {
	if m.Token.IsNull() || m.Token.IsUnknown() {
		return !m.Command.IsNull() || !m.DockerCommand.IsNull()
	}
	if m.Command.IsNull() || m.Command.IsUnknown() || m.DockerCommand.IsNull() || m.DockerCommand.IsUnknown() {
		return true
	}
	command, _ := tunnelCommands(baseURL, fallbackTunnelImageTag, m.Token.ValueString())
	return m.Command.ValueString() != command
}

// refreshToken records the server's preview and drops a cached token that no
// longer matches it, so a stale credential is never exported.
func (m *tunnelResourceModel) refreshToken(preview string, diagnostics *diag.Diagnostics) {
	m.TokenPreview = types.StringValue(preview)
	if m.Token.IsNull() || m.Token.IsUnknown() {
		return
	}
	if !client.IsKnownTunnelTokenPreview(preview) {
		diagnostics.AddWarning("Tunnel Token Could Not Be Verified",
			"Obot returned a token preview in an unrecognized format, so the provider cannot confirm that the token in state is still current. "+
				"The token is kept. Upgrade the provider, or increase token_rotation_version to capture a new token.")
		return
	}
	if previous, err := client.TunnelTokenPreview(m.Token.ValueString()); err != nil || previous != preview {
		m.Token = types.StringNull()
		diagnostics.AddWarning("Tunnel Token Changed Outside Terraform",
			"The cached tunnel token is no longer current and has been removed from state. Obot cannot return the replacement token. "+
				"Increase token_rotation_version to capture a newly rotated token, or keep managing the existing credential outside Terraform.")
	}
}

// setTunnelIdentity records the resource identity when Terraform supports it.
func setTunnelIdentity(ctx context.Context, identity *tfsdk.ResourceIdentity, id types.String, diagnostics *diag.Diagnostics) {
	if identity == nil {
		return
	}
	diagnostics.Append(identity.Set(ctx, tunnelIdentityModel{ID: id})...)
}

func (r *mcpTunnelResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || req.State.Raw.IsNull() {
		return
	}
	var plan, state tunnelResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !plan.TokenRotationVersion.IsUnknown() && plan.TokenRotationVersion.ValueInt64() < state.TokenRotationVersion.ValueInt64() {
		resp.Diagnostics.AddAttributeError(path.Root("token_rotation_version"), "Tunnel Rotation Version Cannot Decrease",
			"Increase token_rotation_version to request rotation; keep its current value for metadata-only updates.")
		return
	}
	if plan.TokenRotationVersion.IsUnknown() || !plan.TokenRotationVersion.Equal(state.TokenRotationVersion) {
		plan.Token = types.StringUnknown()
		plan.TokenPreview = types.StringUnknown()
		plan.Command = types.StringUnknown()
		plan.DockerCommand = types.StringUnknown()
	} else {
		plan.Token = state.Token
		plan.TokenPreview = state.TokenPreview
		plan.Command = state.Command
		plan.DockerCommand = state.DockerCommand
	}
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

func (r *mcpTunnelResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data tunnelResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	manifest := data.manifest(ctx, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	tunnel, err := r.client.CreateTunnel(ctx, manifest)
	if tunnel != nil {
		// The tunnel exists even when its secret response was unusable. Save it
		// so Terraform taints it rather than losing track of the object.
		data.setMetadata(ctx, tunnel, &resp.Diagnostics)
		data.captureToken(tunnel)
		data.setCommands(ctx, r.client, &resp.Diagnostics)
		resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
		setTunnelIdentity(ctx, resp.Identity, data.ID, &resp.Diagnostics)
	}
	if err != nil {
		resp.Diagnostics.AddError("Error Creating MCP Tunnel", err.Error())
	}
}

func (r *mcpTunnelResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data tunnelResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	tunnel, err := r.client.GetTunnel(ctx, data.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Error Reading MCP Tunnel", err.Error())
		return
	}
	data.setMetadata(ctx, tunnel, &resp.Diagnostics)
	data.refreshToken(tunnel.Token, &resp.Diagnostics)
	// Commands keep the image tag captured with the token, so a server
	// upgrade does not change docker_command, but follow base_url, and are
	// cleared with a stale token.
	if data.tokenCommandsStale(r.client.BaseURL()) {
		data.setCommands(ctx, r.client, &resp.Diagnostics)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
	setTunnelIdentity(ctx, resp.Identity, data.ID, &resp.Diagnostics)
}

func (r *mcpTunnelResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state tunnelResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	manifest := plan.manifest(ctx, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if plan.TokenRotationVersion.IsUnknown() || plan.TokenRotationVersion.IsNull() ||
		plan.TokenRotationVersion.ValueInt64() < state.TokenRotationVersion.ValueInt64() {
		resp.Diagnostics.AddError("Invalid Tunnel Rotation Version",
			"The resolved rotation version must be known and must not decrease. Keep its current value for metadata-only updates.")
		return
	}
	if !plan.DisplayName.Equal(state.DisplayName) || !plan.Description.Equal(state.Description) || !plan.AllowedURLs.Equal(state.AllowedURLs) {
		tunnel, err := r.client.UpdateTunnel(ctx, state.ID.ValueString(), manifest)
		if err != nil {
			resp.Diagnostics.AddError("Error Updating MCP Tunnel", err.Error())
			return
		}
		// Token values stay as planned; the next refresh reconciles an external
		// rotation instead of contradicting the plan during apply.
		state.setMetadata(ctx, tunnel, &resp.Diagnostics)
	}
	// Save successful metadata changes before attempting a separate rotation.
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !plan.TokenRotationVersion.Equal(state.TokenRotationVersion) {
		tunnel, err := r.client.RotateTunnelToken(ctx, state.ID.ValueString())
		if tunnel == nil {
			resp.Diagnostics.AddError("Error Rotating MCP Tunnel Token", err.Error())
			return
		}
		// A rotation response always revokes the previous token, so record
		// the outcome even when the new secret was unusable.
		state.captureToken(tunnel)
		state.setCommands(ctx, r.client, &resp.Diagnostics)
		state.TokenRotationVersion = plan.TokenRotationVersion
		if err != nil {
			resp.Diagnostics.AddError("Error Rotating MCP Tunnel Token", err.Error())
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *mcpTunnelResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data tunnelResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteTunnel(ctx, data.ID.ValueString()); err != nil {
		resp.Diagnostics.AddError("Error Deleting MCP Tunnel", err.Error())
	}
}

// ImportState accepts an import ID or an identity. Either way it adopts the
// definition without its secret: token stays null and rotation restarts at 0.
func (r *mcpTunnelResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id := req.ID
	if id == "" && resp.Identity != nil {
		var identity tunnelIdentityModel
		resp.Diagnostics.Append(resp.Identity.Get(ctx, &identity)...)
		id = identity.ID.ValueString()
	}
	if !client.ValidTunnelID(id) {
		resp.Diagnostics.AddError("Invalid MCP Tunnel Import ID", "Import using the generated Obot tunnel ID, not its display name or token.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("token"), types.StringNull())...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("command"), types.StringNull())...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("docker_command"), types.StringNull())...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("token_rotation_version"), int64(0))...)
	setTunnelIdentity(ctx, resp.Identity, types.StringValue(id), &resp.Diagnostics)
	resp.Diagnostics.AddWarning("Tunnel Token Unavailable After Import",
		"The definition is adopted without rotating its credential. Existing clients and secret-manager values are unchanged. "+
			"Obot cannot return the full token; token is null. "+
			"Keep token_rotation_version at 0 to preserve the current credential, or explicitly increase it to rotate and capture a new token.")
}
