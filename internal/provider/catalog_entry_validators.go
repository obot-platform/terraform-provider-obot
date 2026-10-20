// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	// remoteHostnamePattern mirrors Obot's hostname rule for remote catalog entries.
	remoteHostnamePattern = regexp.MustCompile(`^(?:\*\.)?[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)
	// canonicalHeaderKey is an HTTP field name as Obot stores it: upper case,
	// with hyphens in place of underscores.
	canonicalHeaderKey = regexp.MustCompile("^[A-Z0-9!#$%&'*+.^`|~-]+$")
)

// normalizeHeaderKey applies Obot's normalization of header configuration keys.
func normalizeHeaderKey(key string) string {
	return strings.ReplaceAll(strings.ToUpper(key), "_", "-")
}

// headerKey rejects keys Obot would rewrite, which would otherwise appear as
// a change on every plan.
type headerKey struct{}

func (headerKey) Description(context.Context) string {
	return "must be an HTTP header name in upper case, with hyphens rather than underscores, such as X-API-KEY"
}

func (v headerKey) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (v headerKey) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	key := req.ConfigValue.ValueString()
	if canonicalHeaderKey.MatchString(key) {
		return
	}
	detail := "Header key " + v.Description(ctx) + "."
	if normalized := normalizeHeaderKey(key); normalized != key && canonicalHeaderKey.MatchString(normalized) {
		detail = fmt.Sprintf("Obot stores header keys in upper case with hyphens. Use %q instead of %q.", normalized, key)
	}
	resp.Diagnostics.AddAttributeError(req.Path, "Invalid Header Key", detail)
}

// uniqueHeaderKeys rejects duplicate header keys, which Obot also rejects.
type uniqueHeaderKeys struct{}

func (uniqueHeaderKeys) Description(context.Context) string { return "header keys must be unique" }

func (v uniqueHeaderKeys) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (v uniqueHeaderKeys) ValidateList(_ context.Context, req validator.ListRequest, resp *validator.ListResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	seen := map[string]bool{}
	for i, element := range req.ConfigValue.Elements() {
		obj, ok := element.(types.Object)
		if !ok || obj.IsNull() || obj.IsUnknown() {
			continue
		}
		key, ok := obj.Attributes()["key"].(types.String)
		if !ok || key.IsNull() || key.IsUnknown() {
			continue
		}
		if seen[key.ValueString()] {
			resp.Diagnostics.AddAttributeError(req.Path.AtListIndex(i).AtName("key"), "Duplicate Header Key",
				fmt.Sprintf("Header %q is defined more than once.", key.ValueString()))
		}
		seen[key.ValueString()] = true
	}
}

// remoteFixedURL requires an HTTP or HTTPS URL with a host and no credentials.
type remoteFixedURL struct{}

func (remoteFixedURL) Description(context.Context) string {
	return "must be an HTTP or HTTPS URL with a host and without embedded credentials"
}

func (v remoteFixedURL) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (v remoteFixedURL) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	raw := req.ConfigValue.ValueString()
	valid := strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")
	// A ${KEY} reference may supply any part, so Obot validates the URL once filled in.
	if !strings.Contains(raw, "${") {
		u, err := url.Parse(raw)
		valid = err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && u.Hostname() != ""
	}
	if !valid {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid Remote URL", "fixed_url "+v.Description(ctx)+".")
	}
}

// remoteEntryValidator enforces Obot's rule that a tunneled URL is fixed:
// the tunnel's allowlist is checked against it when the entry is saved.
type remoteEntryValidator struct{}

func (remoteEntryValidator) Description(context.Context) string {
	return "remote.fixed_url cannot contain ${...} references when remote.tunnel_id is set"
}

func (v remoteEntryValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v remoteEntryValidator) ValidateResource(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var tunnel, fixedURL types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("remote").AtName("tunnel_id"), &tunnel)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("remote").AtName("fixed_url"), &fixedURL)...)
	if resp.Diagnostics.HasError() || tunnel.IsNull() || fixedURL.IsNull() || fixedURL.IsUnknown() {
		return
	}
	if strings.Contains(fixedURL.ValueString(), "${") {
		resp.Diagnostics.AddAttributeError(path.Root("remote").AtName("fixed_url"), "Tunneled URL Cannot Be a Template",
			"Obot checks a tunneled entry's URL against the tunnel's allowed_urls when the entry is saved, so "+v.Description(ctx)+".")
	}
}
