# Installation

## Home Manager (recommended)

```nix
tinyland.cleanup.enable = true;
```

`home-manager switch` installs the binary, renders
`~/.config/tinyland-cleanup/config.yaml`, and starts the daemon — a launchd
agent on macOS, a systemd user service on Linux. Every tunable is a Nix option;
see [Configuration](configuration.md).

## Nix flake

```sh
nix build github:Jesssullivan/tinyland-cleanup#default
install -Dm755 result/bin/tinyland-cleanup ~/.local/bin/tinyland-cleanup
```

The package authority is the flake output `.#tinyland-cleanup` (aliased
`.#default`).

## RPM (Linux)

Install the `.rpm` from a release:

```sh
sudo dnf install ./tinyland-cleanup-*.rpm
sudo systemctl enable --now tinyland-cleanup   # enable and start the daemon
```

The RPM installs the binary at `/usr/bin/tinyland-cleanup`, a default config at
`/etc/tinyland-cleanup/config.yaml`, and a systemd unit. Enabling the service is
an explicit operator step. See [RPM packaging](rpm-packaging.md).

## From source

```sh
git clone https://github.com/Jesssullivan/tinyland-cleanup
cd tinyland-cleanup
env GOFLAGS=-mod=vendor go build -o tinyland-cleanup .
```

Dependencies are vendored, so no network is needed to build.

## First run

```sh
tinyland-cleanup --once --dry-run --level critical
```

This plans a cleanup without changing anything, using built-in defaults when no
config exists. See [Usage](usage.md) for run modes and the daemon lifecycle.
