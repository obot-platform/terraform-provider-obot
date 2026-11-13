# Releasing

Releases are built by GoReleaser from a `v*` tag (`.github/workflows/release.yml`)
and published to the Terraform Registry as `obot-platform/obot`.

## One-time setup

### 1. Move the repository to the organization

Transfer the repository to `obot-platform` (**Settings > General > Transfer**),
keeping the name `terraform-provider-obot`. The Registry requires a public
repository named `terraform-provider-{name}` in lowercase. GitHub redirects the
old URL, but update local clones:

```shell
git remote set-url origin git@github.com:obot-platform/terraform-provider-obot.git
```

Confirm that Actions are enabled for the repository after the transfer, and that
the Tests and Acceptance workflows pass on `main`.

### 2. Create a signing key

The Registry accepts RSA or DSA keys, not GPG's default ECC type. Create a
dedicated RSA key for the organization:

```shell
gpg --full-generate-key
# Kind: (1) RSA and RSA, size 4096, an expiry you will track,
# name "Obot Terraform Provider", an organization address, and a passphrase.

gpg --list-secret-keys --keyid-format long   # note the key ID
gpg --armor --export KEY_ID > obot-terraform-public.asc
gpg --armor --export-secret-keys KEY_ID > obot-terraform-private.asc
```

Store the private key and passphrase in the organization's secret manager. Delete
`obot-terraform-private.asc` after adding it to GitHub.

### 3. Add the GitHub secrets

In the transferred repository (**Settings > Secrets and variables > Actions**),
add:

| Secret | Value |
| --- | --- |
| `GPG_PRIVATE_KEY` | Contents of `obot-terraform-private.asc` |
| `PASSPHRASE` | The key's passphrase |

### 4. Set up the Registry namespace

1. Sign in to <https://registry.terraform.io> with a GitHub account that is an
   admin of `obot-platform`.
2. If the organization restricts OAuth apps, approve the **Terraform Registry**
   app for `obot-platform` (**Organization settings > Third-party access**). The
   app needs access to the provider repository.
3. Under **User Settings > Signing Keys**, add the contents of
   `obot-terraform-public.asc` to the `obot-platform` namespace.

## Releasing a version

1. Check that the Tests and Acceptance workflows pass on `main`.
2. In `CHANGELOG.md`, replace `(Unreleased)` with the release date, for example
   `## 0.1.0 (October 20, 2026)`, and commit.
3. Tag and push:

   ```shell
   git tag v0.1.0
   git push origin v0.1.0
   ```

4. Watch the Release workflow. The GitHub release must contain a zip per
   platform, `terraform-provider-obot_0.1.0_manifest.json`,
   `terraform-provider-obot_0.1.0_SHA256SUMS`, and
   `terraform-provider-obot_0.1.0_SHA256SUMS.sig`.
5. **First release only:** in the Registry, choose **Publish > Provider**, select
   `obot-platform` and `terraform-provider-obot`, and publish. This creates a
   webhook on the repository, so later releases are ingested automatically.
6. Confirm the version appears at
   <https://registry.terraform.io/providers/obot-platform/obot> and that
   `terraform init` installs it with:

   ```hcl
   terraform {
     required_providers {
       obot = {
         source  = "obot-platform/obot"
         version = "0.1.0"
       }
     }
   }
   ```

7. **First release only:** list the provider in the OpenTofu Registry, as
   described under [OpenTofu Registry](#opentofu-registry).
8. Add a new `## X.Y.Z (Unreleased)` heading to `CHANGELOG.md` for the next release.

If a version does not appear, check the provider's settings page in the Registry.
A missing webhook is fixed by deleting any `registry.terraform.io` webhooks on
the repository and choosing **Resync**. A signature error means the public key in
the namespace does not match `GPG_PRIVATE_KEY`.

To check the build without signing or publishing:

```shell
goreleaser release --snapshot --clean --skip=sign,publish
```

## OpenTofu Registry

OpenTofu resolves `obot-platform/obot` to `registry.opentofu.org`, a separate
index of the same GitHub releases. CI already tests the provider under OpenTofu;
listing it is a one-time step after the first Terraform Registry release:

1. Make your `obot-platform` membership public on GitHub
   (on <https://github.com/orgs/obot-platform/people>, set your
   visibility to **Public**). The key submission fails
   automated validation otherwise.
2. Open the [Submit new Provider](https://github.com/opentofu/registry/issues/new?template=provider.yml)
   form and enter `obot-platform/terraform-provider-obot`.
3. Open the [Submit new Provider Signing Key](https://github.com/opentofu/registry/issues/new?template=provider_key.yml)
   form with namespace `obot-platform` and the contents of
   `obot-terraform-public.asc`.

Use the web forms: the OpenTofu registry closes submissions created with `gh`
or the API. Once both issues are processed, later releases are picked up from
GitHub automatically. Confirm with `tofu init` and the same `required_providers`
block as above, without development overrides.

Neither registry removes a published version, so a broken release is fixed by
releasing a new one. Consider enabling GitHub's immutable releases on the
repository.
