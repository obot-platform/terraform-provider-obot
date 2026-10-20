// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/obot-platform/terraform-provider-obot/internal/client"
)

// tunnelImage is the Obot image that runs obot tunnel without a local install.
const tunnelImage = "ghcr.io/obot-platform/obot"

// fallbackTunnelImageTag is the image tag the Obot UI uses when the server
// version does not name a published release.
const fallbackTunnelImageTag = "main"

// These match the Obot UI's MCPTunnelSecretRevealDialog, so Terraform and the
// UI suggest the same image for a server.
var (
	semverPattern = regexp.MustCompile(`^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
		`(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?` +
		`(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)
	developmentVersionPattern = regexp.MustCompile(`^v?0\.0\.0-dev(?:[-.+]|$)`)
	// shellSafe matches words a POSIX shell, zsh, or PowerShell passes through
	// unchanged, so typical URLs appear exactly as the UI shows them.
	shellSafe = regexp.MustCompile(`^[A-Za-z0-9%+,./:=@_~-]+$`)
)

// tunnelImageTag derives the Obot image tag for a server version: the release
// version without build metadata, or main for missing, unparseable,
// development, and dirty builds.
func tunnelImageTag(version string) string {
	version = strings.TrimSpace(version)
	if !semverPattern.MatchString(version) || developmentVersionPattern.MatchString(version) || strings.HasSuffix(version, "-dirty") {
		return fallbackTunnelImageTag
	}
	tag, _, _ := strings.Cut(version, "+")
	return tag
}

// shellWord single-quotes a value only when a shell could otherwise change it.
func shellWord(value string) string {
	if shellSafe.MatchString(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// tunnelCommands formats the commands the Obot UI shows after creation or
// rotation.
func tunnelCommands(baseURL, tag, token string) (command, dockerCommand string) {
	args := "tunnel --obot-base-url " + shellWord(baseURL) + " --token " + shellWord(token)
	return "obot " + args, "docker run --rm " + tunnelImage + ":" + shellWord(tag) + " " + args
}

// setCommands derives command and docker_command from the token in the model.
// They are null whenever the token is. Without a token no request is made. A
// server version that cannot be read is a warning and uses the main image,
// because the tunnel and its token already exist.
func (m *tunnelResourceModel) setCommands(ctx context.Context, conn *client.Client, diagnostics *diag.Diagnostics) {
	if m.Token.IsNull() || m.Token.IsUnknown() || conn == nil {
		m.Command = types.StringNull()
		m.DockerCommand = types.StringNull()
		return
	}
	version, err := conn.ServerVersion(ctx)
	if err != nil {
		diagnostics.AddWarning("Obot Version Unavailable for Tunnel Command",
			"The provider could not read the Obot server version, so docker_command uses the "+tunnelImage+":"+fallbackTunnelImageTag+
				" image. Edit the image tag to match your Obot release if needed. "+err.Error())
	}
	command, dockerCommand := tunnelCommands(conn.BaseURL(), tunnelImageTag(version), m.Token.ValueString())
	m.Command = types.StringValue(command)
	m.DockerCommand = types.StringValue(dockerCommand)
}
