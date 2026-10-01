# Releasing ah-mcp

Releases are **manual only**. Pushing to `master` runs CI but never publishes
anything. Publishing is done by `.github/workflows/release.yml`, which:

- validates the tag (`^v\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$`);
- builds `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64` binaries
  (`CGO_ENABLED=0`, `-trimpath`, version = tag, commit = release commit) and
  uploads `ah-mcp_<version>_<os>_<arch>.tar.gz` (binary + `LICENSE` +
  `README.md`) plus `SHA256SUMS` to the GitHub Release, with a build-provenance
  attestation;
- builds and pushes the multi-arch image `ghcr.io/fedorov-xyz/ah-mcp`
  (`linux/amd64,linux/arm64`, SBOM, provenance `mode=max`, attested).

Image tags:

| Release | Tags |
|---|---|
| `v1.4.2` | `1.4.2`, `1.4`, `1`, `latest` |
| `v1.5.0-rc.1` (pre-release) | `1.5.0-rc.1` only — `latest` does not move |

## Cut a release

1. Make sure CI on `master` is green and `UPSTREAM.md` / `README.md` are up to date.
2. Create the release (this creates the tag and triggers the workflow):

   ```sh
   gh release create v0.1.0 --generate-notes
   # pre-release:
   gh release create v0.2.0-rc.1 --generate-notes --prerelease
   ```

   Or in the UI: **Releases → Draft a new release → choose a new tag `vX.Y.Z` → Publish release**.
3. Watch **Actions → release**.

## Re-run / re-publish an existing release

**Actions → release → Run workflow**, enter the existing tag (e.g. `v0.1.0`)
→ **Run workflow**. Or:

```sh
gh workflow run release.yml -f tag=v0.1.0
```

The release must already exist. Binaries are re-uploaded with `--clobber`;
image tags are overwritten with the rebuilt image.

## Verify a release

```sh
VER=0.1.0
gh release download "v$VER" --repo fedorov-xyz/ah-mcp
sha256sum -c SHA256SUMS            # macOS: shasum -a 256 -c SHA256SUMS

gh attestation verify "ah-mcp_${VER}_linux_amd64.tar.gz" --repo fedorov-xyz/ah-mcp
gh attestation verify "oci://ghcr.io/fedorov-xyz/ah-mcp:${VER}" --repo fedorov-xyz/ah-mcp

docker run --rm "ghcr.io/fedorov-xyz/ah-mcp:${VER}" --version
```

## One-time step after the very first publish

A new GHCR package is always created **private**, and its visibility can only
be changed in the GitHub UI. After the first release has pushed the image:

1. Open <https://github.com/users/fedorov-xyz/packages/container/ah-mcp/settings>.
2. **Danger Zone → Change visibility → Public**.
3. Ideally, under **Manage Actions access / Repository source**, link the
   package to the `fedorov-xyz/ah-mcp` repository (the
   `org.opencontainers.image.source` label usually does this automatically).

This setting persists for all later versions.
