// SPDX-License-Identifier: MPL-2.0

package client

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestCatalogEntryUnmodeled(t *testing.T) {
	for name, test := range map[string]struct {
		manifest string
		want     []string
	}{
		"modeled and Obot-owned fields": {`{"name": "A", "runtime": "remote", "metadata": {"categories": "x"},
			"remoteConfig": {"fixedURL": "http://localhost:3000/mcp", "tunnelName": "mt1abc", "staticOAuthRequired": true},
			"config": [{"key": "X-API-KEY", "static": true, "usage": "header", "sensitive": true}],
			"toolPreview": [{"name": "search"}], "staticConfigurationRevision": "r1"}`, nil},
		"empty values": {`{"name": "A", "runtime": "remote", "entryKey": "", "resources": {"requests": {}, "limits": {}},
			"remoteConfig": {"fixedURL": "https://a.example", "localhostCallbackEnabled": false},
			"config": [{"key": "X-A", "usage": "header", "options": [], "secretBinding": null}]}`, nil},
		"URL template": {`{"runtime": "remote", "remoteConfig": {"urlTemplate": "https://${TENANT}.example.com"},
			"config": [{"key": "TENANT", "usage": "env", "required": true}]}`,
			[]string{"remoteConfig.urlTemplate", `config["TENANT"] with usage "env"`}},
		"header extras": {`{"runtime": "remote", "config": [{"key": "X-A", "usage": "header",
			"secretBinding": {"name": "s", "key": "k"}, "options": [{"name": "o", "value": "v"}]}]}`,
			[]string{`config["X-A"].options`, `config["X-A"].secretBinding`}},
		"other fields": {`{"runtime": "remote", "resources": {"limits": {"memory": "1Gi"}}, "upgradeNote": "n",
			"remoteConfig": {"hostname": "a.example", "localhostCallbackEnabled": true, "localhostCallbackPath": "/cb"}}`,
			[]string{"resources", "upgradeNote", "remoteConfig.localhostCallbackEnabled", "remoteConfig.localhostCallbackPath"}},
	} {
		t.Run(name, func(t *testing.T) {
			var entry CatalogEntryDefinition
			if err := json.Unmarshal([]byte(`{"id": "default-a", "manifest": `+test.manifest+`}`), &entry); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(entry.Unmodeled, test.want) {
				t.Fatalf("unmodeled = %q, want %q", entry.Unmodeled, test.want)
			}
			if entry.ID != "default-a" || entry.Manifest.Runtime != "remote" {
				t.Fatalf("entry not decoded: %+v", entry)
			}
		})
	}
}

func TestCatalogEntryRedaction(t *testing.T) {
	const secret = "header-secret-value"
	config := CatalogEntryConfig{Key: "X-API-KEY", Value: secret, Usage: HeaderUsage}
	manifest := CatalogEntryManifest{Name: "A", Config: []CatalogEntryConfig{config}}
	entry := CatalogEntryDefinition{ID: "default-a", Manifest: manifest}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, value := range []any{config, manifest, entry, &entry, []CatalogEntryConfig{config}} {
			if out := fmt.Sprintf(format, value); strings.Contains(out, secret) {
				t.Errorf("%s of %T leaks the header value: %s", format, value, out)
			}
		}
	}
}

func TestDisplaySourceURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://github.com/example/catalog":                               "https://github.com/example/catalog",
		"https://x-access-token:ghp_abc123@github.com/example/catalog.git": "https://github.com/example/catalog.git",
		"https://user@github.com/example/catalog?token=abc#frag":           "https://github.com/example/catalog",
		"":                    "a catalog source",
		"not a url":           "a catalog source",
		"https://%zz@example": "a catalog source",
	} {
		if got := DisplaySourceURL(raw); got != want {
			t.Errorf("DisplaySourceURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestStripURLCredentials(t *testing.T) {
	for raw, want := range map[string]struct {
		url     string
		secrets []string
	}{
		"https://github.com/example/catalog/tree/main/mcp": {"https://github.com/example/catalog/tree/main/mcp", nil},
		"https://x-access-token:ghp_pw@github.com/example/c.git": {"https://github.com/example/c.git",
			[]string{"x-access-token:ghp_pw", "x-access-token", "ghp_pw"}},
		"https://ghp_token@github.com/example/c#mcp": {"https://github.com/example/c#mcp", []string{"ghp_token"}},
		// Obot's messages repeat the raw, still-encoded form.
		"https://tok%2Den@github.com/example/c": {"https://github.com/example/c", []string{"tok%2Den", "tok-en"}},
		// Query values can be working credentials for non-Git sources.
		"https://gitlab.example/api/v4/raw?private_token=glpat%2Dsecret&ref=main&flag": {
			"https://gitlab.example/api/v4/raw?private_token=REDACTED&ref=REDACTED&flag",
			[]string{"glpat%2Dsecret", "glpat-secret", "main"}},
		"":                 {"", nil},
		"https://%zz@host": {"https://%zz@host", nil},
	} {
		got, secrets := StripURLCredentials(raw)
		if got != want.url || !slices.Equal(secrets, want.secrets) {
			t.Errorf("StripURLCredentials(%q) = %q, %q; want %q, %q", raw, got, secrets, want.url, want.secrets)
		}
	}
}
