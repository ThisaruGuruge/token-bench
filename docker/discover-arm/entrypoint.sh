#!/bin/sh
# Reads the API key from the first line of stdin into a tmpfs file only the
# container user can read, then hands the rest of stdin to claude. The key is
# never an argument, an environment variable or a file on disk.
set -eu
umask 077
IFS= read -r key || key=""
if [ -n "$key" ]; then
  printf '%s' "$key" > /run/bench-secret/key
fi
key=""
exec claude "$@"
