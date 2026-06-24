#!/usr/bin/env python3
"""Bazel runner: build the MkDocs site into a Bazel-declared output directory.

This is the Python analogue of site.scaffold's run-vite-build.mjs. It forces a
deterministic build epoch and writes to the --site-dir path Bazel provides,
which is the declared TreeArtifact output.
"""
import os
import subprocess
import sys

# Deterministic build: stable epoch so any timestamped asset is reproducible and
# byte-identical with the Nix build (which sets the same value).
os.environ.setdefault("SOURCE_DATE_EPOCH", "315532800")  # 1980-01-01
# Keep mkdocs/material caches inside the sandbox.
os.environ.setdefault("HOME", os.environ.get("TMPDIR", "/tmp"))

args = sys.argv[1:]
config = args[args.index("--config") + 1]
site_dir = os.path.abspath(args[args.index("--site-dir") + 1])

sys.exit(
    subprocess.call(
        [
            sys.executable,
            "-m",
            "mkdocs",
            "build",
            "--strict",
            "--config-file",
            config,
            "--site-dir",
            site_dir,
        ]
    )
)
