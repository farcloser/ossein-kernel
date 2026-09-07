# ossein-kernel

This project is a linux 7.x _kernel image builder for macOS_.

It does provide a baseline configuration and patches that are specifically
tailored for use in the context of Apple VZ, focused on performance and minimal footprint.

As such, the primary intended consumer of these kernels is [Ossein](https://github.com/farcloser/ossein),
though it is not limited to this, as users can point to their own configuration, and elect to different
patches to build any kernel as they see fit.

The baseline setup produces kernel images under 10MB, that are outperforming or
tie-ing kernels from other macOS container runtimes.

## How to build it

### Requirements

* macOS 26+
* aqua installed

```
TBD
```

## Layout

Stock kernel configuration fragments live under kernel/config.

Patches live under kernel/patches.

The kernel builder logic is in cmd/ossein-kernel/main.go, and the actual in-container
build script is in cmd/ossein-kernel/build.sh

## Releasing

The kernel is the product; the builder and init are not published. A release is cut
locally: the build boots a real VM on Virtualization.framework, which no stock CI runner
can, so there is no release workflow.

```
just release-kernel X.Y.Z-ossein.N
```

Run it from a clean tree. `X.Y.Z` must be the upstream version pinned in
`kernel_source_url` (the recipe refuses anything else); bump `N` for a rebuild of the same
upstream (config, patch or toolchain change) and reset it to 1 on a source bump. No leading
`v`: a `v`-prefixed, hyphen-suffixed tag parses as a SemVer pre-release, sorts before
`X.Y.Z`, and loses GitHub's "Latest". The recipe builds and boot-verifies the kernel, signs
the checksums keyless with cosign (a browser opens for the OIDC login), creates a signed
git tag, pushes it, and publishes `kernel-arm64`, `perf-arm64`, `SHA256SUMS` and
`SHA256SUMS.cosign.bundle` to a GitHub release.

Consumers verify a release with:

```
cosign verify-blob --bundle SHA256SUMS.cosign.bundle \
  --certificate-identity-regexp '^(142371135\+[^@]+@users\.noreply\.github\.com|apostasie@farcloser\.world)$' \
  --certificate-oidc-issuer https://github.com/login/oauth SHA256SUMS
shasum -a 256 -c SHA256SUMS
```
