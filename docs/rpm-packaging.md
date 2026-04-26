# RPM Packaging

The RPM package is a Linux/Rocky distribution surface for hosts that should not
consume the Nix flake directly. Nix remains the local tool authority: release
CI invokes `nfpm` through `nix shell nixpkgs#nfpm`.

Tagged releases produce:

- cross-platform tarballs;
- Linux RPMs for `amd64` and `arm64`;
- `SHA256SUMS` covering all release artifacts.

The RPM installs:

- `/usr/bin/tinyland-cleanup`;
- `/etc/tinyland-cleanup/config.yaml` as a noreplace config file;
- `/usr/lib/systemd/system/tinyland-cleanup.service`;
- `/var/lib/tinyland-cleanup`;
- `/var/log/tinyland-cleanup`.

The systemd service is installed but not enabled or started automatically.
Operators should review the config and run a dry-run first:

```sh
tinyland-cleanup --once --dry-run --level critical --output json --config /etc/tinyland-cleanup/config.yaml
```

Enable the service only after the dry-run plan matches the host policy:

```sh
systemctl enable --now tinyland-cleanup.service
```

Build an RPM locally with:

```sh
mkdir -p dist/package-test
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/package-test/tinyland-cleanup .
NFPM_BINARY=dist/package-test/tinyland-cleanup \
  NFPM_VERSION=0.2.0 \
  NFPM_RELEASE=1 \
  NFPM_ARCH=amd64 \
  nix shell nixpkgs#nfpm --command nfpm package --packager rpm --config packaging/nfpm.yaml --target dist/package-test
```

The RPM config uses Linux defaults. Darwin launchd packaging and Home Manager
module ingestion remain separate distribution surfaces.
