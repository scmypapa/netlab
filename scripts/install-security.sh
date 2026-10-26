#!/usr/bin/env bash
# Installed node updaters invoke this fixed path from the new release.
exec bash "$(dirname -- "${BASH_SOURCE[0]}")/install-node-runtime.sh"
