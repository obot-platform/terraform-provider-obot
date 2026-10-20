// SPDX-License-Identifier: MPL-2.0

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
)

// HeaderUsage marks catalog entry configuration sent as an HTTP header.
const HeaderUsage = "header"

// RemoteRuntime is the catalog entry runtime for MCP servers reached by URL.
const RemoteRuntime = "remote"

// CatalogEntryConfig is one configuration field of a catalog entry. Obot moves
// a submitted Value into a credential and returns the field with an empty
// Value and Static set. Submitting Static without a Value keeps the stored one.
type CatalogEntryConfig struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Value       string `json:"value"`
	Static      bool   `json:"static,omitempty"`
	Sensitive   bool   `json:"sensitive"`
	Required    bool   `json:"required"`
	Prefix      string `json:"prefix,omitempty"`
	Usage       string `json:"usage"`
}

// String prevents a header value appearing when a field is formatted.
func (c CatalogEntryConfig) String() string {
	return fmt.Sprintf("catalog entry config (%s, %s, value=[REDACTED])", c.Key, c.Usage)
}

// GoString prevents a header value appearing in %#v formatting.
func (c CatalogEntryConfig) GoString() string { return c.String() }

// RemoteCatalogConfig describes how a remote catalog entry is reached.
type RemoteCatalogConfig struct {
	FixedURL            string `json:"fixedURL,omitempty"`
	TunnelName          string `json:"tunnelName,omitempty"`
	Hostname            string `json:"hostname,omitempty"`
	StaticOAuthRequired bool   `json:"staticOAuthRequired,omitempty"`
}

// CatalogEntryManifest is the user-defined part of a catalog entry. An update
// replaces the whole stored manifest except toolPreview and
// staticConfigurationRevision, which Obot owns, so any other field this
// struct omits is erased by an update. CatalogEntryDefinition.Unmodeled
// lists such fields in a response so callers can refuse to update them away.
type CatalogEntryManifest struct {
	Name             string               `json:"name"`
	ShortDescription string               `json:"shortDescription"`
	Description      string               `json:"description"`
	Icon             string               `json:"icon"`
	RepoURL          string               `json:"repoURL,omitempty"`
	Metadata         map[string]string    `json:"metadata,omitempty"`
	Runtime          string               `json:"runtime"`
	RemoteConfig     *RemoteCatalogConfig `json:"remoteConfig,omitempty"`
	Config           []CatalogEntryConfig `json:"config,omitempty"`
	// StaticConfigurationRevision identifies the stored header values. Obot
	// assigns a new one whenever the values change, however they are written,
	// and ignores it in requests.
	StaticConfigurationRevision string `json:"staticConfigurationRevision,omitempty"`
}

// String prevents header values appearing when a manifest is formatted.
func (m CatalogEntryManifest) String() string {
	return fmt.Sprintf("catalog entry manifest (%s, %d config fields, values=[REDACTED])", m.Name, len(m.Config))
}

// GoString prevents header values appearing in %#v formatting.
func (m CatalogEntryManifest) GoString() string { return m.String() }

// DisplaySourceURL returns a catalog source URL safe to show in diagnostics.
// Obot accepts source URLs with embedded credentials, such as a Git token as
// userinfo, and copies them onto synchronized entries unchanged.
func DisplaySourceURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "a catalog source"
	}
	u.User = nil
	u.RawQuery, u.Fragment = "", ""
	return u.String()
}

// StripURLCredentials removes credentials from a source URL before it is
// shown or stored: userinfo, such as a Git token, and every query value, since
// Obot fetches non-Git sources with the query intact and a value such as
// private_token or a presigned signature is a working credential. Query names
// are kept. It also returns each removed credential, raw and decoded, so
// callers can scrub them from messages that repeat the URL. A URL that does
// not parse is returned unchanged; Obot stores only source URLs that parse.
func StripURLCredentials(raw string) (string, []string) {
	u, err := url.Parse(raw)
	if err != nil {
		return raw, nil
	}
	var secrets []string
	add := func(values ...string) {
		for _, value := range values {
			if value != "" && !slices.Contains(secrets, value) {
				secrets = append(secrets, value)
			}
		}
	}
	if u.User != nil {
		// The raw userinfo can be percent-encoded differently from Go's form.
		if _, authority, ok := strings.Cut(raw, "://"); ok {
			if end := strings.IndexAny(authority, "/?#"); end >= 0 {
				authority = authority[:end]
			}
			if at := strings.LastIndex(authority, "@"); at >= 0 {
				user, password, _ := strings.Cut(authority[:at], ":")
				add(authority[:at], user, password)
			}
		}
		password, _ := u.User.Password()
		add(u.User.String(), u.User.Username(), password)
		u.User = nil
	}
	if u.RawQuery != "" {
		pairs := strings.Split(u.RawQuery, "&")
		for i, pair := range pairs {
			name, value, ok := strings.Cut(pair, "=")
			if !ok || value == "" {
				continue
			}
			decoded, err := url.QueryUnescape(value)
			if err != nil {
				decoded = value
			}
			add(value, decoded)
			pairs[i] = name + "=REDACTED"
		}
		u.RawQuery = strings.Join(pairs, "&")
	}
	return u.String(), secrets
}

// CatalogEntryDefinition is a complete catalog entry. Only Editable entries,
// those created through the API rather than synchronized from a Git source,
// can be changed or deleted.
type CatalogEntryDefinition struct {
	ID          string               `json:"id"`
	CatalogName string               `json:"catalogName"`
	SourceURL   string               `json:"sourceURL"`
	Editable    bool                 `json:"editable"`
	Detached    bool                 `json:"detached"`
	Manifest    CatalogEntryManifest `json:"manifest"`
	// Unmodeled names the nonempty manifest content that CatalogEntryManifest
	// cannot represent, such as a URL template, environment configuration, or
	// a header's secret binding. Updating the entry would erase it.
	Unmodeled []string `json:"-"`
}

// Manifest fields CatalogEntryManifest represents, and those Obot owns and
// keeps across updates.
var (
	modeledManifestFields = map[string]bool{
		"name": true, "shortDescription": true, "description": true, "icon": true, "repoURL": true,
		"metadata": true, "runtime": true, "remoteConfig": true, "config": true,
		"toolPreview": true, "staticConfigurationRevision": true,
	}
	modeledRemoteFields = map[string]bool{"fixedURL": true, "tunnelName": true, "hostname": true, "staticOAuthRequired": true}
	modeledConfigFields = map[string]bool{
		"key": true, "name": true, "description": true, "value": true, "static": true,
		"sensitive": true, "required": true, "prefix": true, "usage": true,
	}
)

// UnmarshalJSON decodes an entry and records its unmodeled content.
func (e *CatalogEntryDefinition) UnmarshalJSON(data []byte) error {
	type plain CatalogEntryDefinition
	if err := json.Unmarshal(data, (*plain)(e)); err != nil {
		return err
	}
	var raw struct {
		Manifest map[string]any `json:"manifest"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	e.Unmodeled = unmodeled("", raw.Manifest, modeledManifestFields)
	remote, _ := raw.Manifest["remoteConfig"].(map[string]any)
	e.Unmodeled = append(e.Unmodeled, unmodeled("remoteConfig.", remote, modeledRemoteFields)...)
	configs, _ := raw.Manifest["config"].([]any)
	for _, item := range configs {
		config, _ := item.(map[string]any)
		key, _ := config["key"].(string)
		prefix := fmt.Sprintf("config[%q]", key)
		if usage, _ := config["usage"].(string); usage != HeaderUsage {
			e.Unmodeled = append(e.Unmodeled, fmt.Sprintf("%s with usage %q", prefix, usage))
			continue
		}
		e.Unmodeled = append(e.Unmodeled, unmodeled(prefix+".", config, modeledConfigFields)...)
	}
	return nil
}

// unmodeled returns the sorted names of nonempty fields not in modeled.
func unmodeled(prefix string, fields map[string]any, modeled map[string]bool) []string {
	var names []string
	for name, value := range fields {
		if !modeled[name] && !emptyJSON(value) {
			names = append(names, prefix+name)
		}
	}
	sort.Strings(names)
	return names
}

// emptyJSON reports whether a decoded JSON value carries no information, as
// Obot omits or zeroes fields that are not set.
func emptyJSON(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case bool:
		return !v
	case float64:
		return v == 0
	case []any:
		for _, item := range v {
			if !emptyJSON(item) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, item := range v {
			if !emptyJSON(item) {
				return false
			}
		}
		return true
	}
	return false
}

func catalogEntryPath(catalogID, id string) (string, error) {
	path, err := catalogPath(catalogID)
	if err != nil {
		return "", err
	}
	path += "/entries"
	if id != "" {
		if !ValidObjectID(id) {
			return "", fmt.Errorf("invalid MCP catalog entry ID")
		}
		path += "/" + id
	}
	return path, nil
}

// checkCatalogEntry rejects a response for another object or catalog.
func checkCatalogEntry(entry *CatalogEntryDefinition, catalogID, id string) error {
	if !ValidObjectID(entry.ID) {
		// Name the ID so an operator can remove an object the provider cannot track.
		return fmt.Errorf("obot returned an invalid MCP catalog entry ID %q; remove it in Obot if it was created", entry.ID)
	}
	if (id != "" && entry.ID != id) || entry.CatalogName != catalogID {
		return fmt.Errorf("obot returned an unexpected or incorrectly scoped MCP catalog entry")
	}
	return nil
}

// GetCatalogEntry reads one entry, including its configuration but no stored values.
func (c *Client) GetCatalogEntry(ctx context.Context, catalogID, id string) (*CatalogEntryDefinition, error) {
	path, err := catalogEntryPath(catalogID, id)
	if err != nil {
		return nil, err
	}
	var result CatalogEntryDefinition
	if err := c.request(ctx, http.MethodGet, path, "", &result); err != nil {
		return nil, err
	}
	if err := checkCatalogEntry(&result, catalogID, id); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) writeCatalogEntry(ctx context.Context, catalogID, id string, manifest CatalogEntryManifest) (*CatalogEntryDefinition, error) {
	path, err := catalogEntryPath(catalogID, id)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encoding catalog entry manifest: %w", err)
	}
	method := http.MethodPut
	if id == "" {
		method = http.MethodPost
	}
	var result CatalogEntryDefinition
	if err := c.request(ctx, method, path, string(body), &result); err != nil {
		return nil, err
	}
	if err := checkCatalogEntry(&result, catalogID, id); err != nil {
		return nil, err
	}
	return &result, nil
}

// CreateCatalogEntry creates an editable entry. Obot generates its ID.
func (c *Client) CreateCatalogEntry(ctx context.Context, catalogID string, manifest CatalogEntryManifest) (*CatalogEntryDefinition, error) {
	return c.writeCatalogEntry(ctx, catalogID, "", manifest)
}

// UpdateCatalogEntry replaces an editable entry's definition, keeping its ID.
func (c *Client) UpdateCatalogEntry(ctx context.Context, catalogID, id string, manifest CatalogEntryManifest) (*CatalogEntryDefinition, error) {
	if id == "" {
		return nil, fmt.Errorf("invalid MCP catalog entry ID")
	}
	return c.writeCatalogEntry(ctx, catalogID, id, manifest)
}

// DeleteCatalogEntry requests removal of an editable entry, tolerating prior
// deletion. Obot finishes removing it asynchronously.
func (c *Client) DeleteCatalogEntry(ctx context.Context, catalogID, id string) error {
	if id == "" {
		return fmt.Errorf("invalid MCP catalog entry ID")
	}
	path, err := catalogEntryPath(catalogID, id)
	if err != nil {
		return err
	}
	err = c.request(ctx, http.MethodDelete, path, "", nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}
