// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/obot-platform/terraform-provider-obot/internal/client"
)

var _ provider.Provider = &ObotProvider{}

// ObotProvider implements the Obot Terraform provider.
type ObotProvider struct {
	version string
}

type providerModel struct {
	BaseURL types.String `tfsdk:"base_url"`
	Token   types.String `tfsdk:"token"`
}

func (p *ObotProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "obot"
	resp.Version = p.version
}

func (p *ObotProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Connects to Obot using an API-scoped key. Set `OBOT_BASE_URL` and obtain `OBOT_TOKEN` with `export OBOT_TOKEN=\"$(obot login --scope api --print-token)\"`. Explicit provider configuration takes precedence over environment variables. See the [catalog and access-policy guide](guides/catalogs-and-access-policies.md) for managing catalog sources and the Everything policy, and the [tunnel guide](guides/mcp-tunnels.md) for tunnel credentials and import.",
		Attributes: map[string]schema.Attribute{
			"base_url": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Obot app URL or API base URL. Falls back to `OBOT_BASE_URL`. A trailing `/api` is added when omitted.",
			},
			"token": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "Obot API key with API access. Falls back to `OBOT_TOKEN`. Obtain it with `obot login --scope api --print-token`.",
			},
		},
	}
}

func (p *ObotProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var data providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if data.BaseURL.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("base_url"), "Unknown Obot URL",
			"Set base_url to a known value or use OBOT_BASE_URL; an unknown value cannot fall back to the environment.")
	}
	if data.Token.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("token"), "Unknown Obot Token",
			"Set token to a known value or use OBOT_TOKEN; an unknown value cannot fall back to the environment.")
	}
	if resp.Diagnostics.HasError() {
		return
	}
	baseURL, token := os.Getenv("OBOT_BASE_URL"), os.Getenv("OBOT_TOKEN")
	if !data.BaseURL.IsNull() {
		baseURL = data.BaseURL.ValueString()
	}
	if !data.Token.IsNull() {
		token = data.Token.ValueString()
	}
	if strings.TrimSpace(baseURL) == "" {
		resp.Diagnostics.AddAttributeError(path.Root("base_url"), "Missing Obot URL",
			"No Obot URL was found in base_url or OBOT_BASE_URL. Set one to the Obot app or API URL.")
	}
	if strings.TrimSpace(token) == "" {
		resp.Diagnostics.AddAttributeError(path.Root("token"), "Missing Obot Token",
			"No credentials were found in token or OBOT_TOKEN. Run: export OBOT_TOKEN=\"$(obot login --scope api --print-token)\".")
	}
	if resp.Diagnostics.HasError() {
		return
	}
	userAgent := fmt.Sprintf("terraform-provider-obot/%s (+https://registry.terraform.io/providers/obot-platform/obot) Terraform/%s", p.version, req.TerraformVersion)
	conn, err := client.New(baseURL, token, userAgent)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("base_url"), "Invalid Obot URL", err.Error())
		return
	}
	if err := conn.Validate(ctx); err != nil {
		resp.Diagnostics.AddError("Unable to Connect to Obot", err.Error())
		return
	}
	resp.DataSourceData = conn
	resp.ResourceData = conn
}

func (p *ObotProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{newMCPTunnelResource, newMCPCatalogSourceResource, newMCPAccessRuleResource, newGitCredentialResource, newMCPCatalogEntryResource}
}

func (p *ObotProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{newCurrentUserDataSource, newMCPTunnelDataSource, newMCPTunnelsDataSource, newMCPCatalogDataSource, newCurrentAuthProviderDataSource, newMCPAccessRuleDataSource, newMCPAccessRulesDataSource, newGitCredentialDataSource, newGitCredentialsDataSource}
}

// New returns a factory for the Obot provider at the given version.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &ObotProvider{
			version: version,
		}
	}
}
