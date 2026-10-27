# Keep this the first recipe: bare `just` lists recipes and starts no Bazel
# server (TIN-5770, R472). Bazel runs through the flywheel-* recipes below.
# List available commands
default:
    @just --list

import? "justfile.flywheel"
