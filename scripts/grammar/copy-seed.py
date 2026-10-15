"""Copies a discover cache seed and re-stamps its latest-version entries with
now, exactly as the runner does for each run (arm.CopySeed).
usage: python3 copy-seed.py <seed> <destination>"""
import json, os, shutil, sys, time

seed, destination = sys.argv[1], sys.argv[2]
now = int(time.time() * 1000)
shutil.copytree(seed, destination)
for directory, _, files in os.walk(os.path.join(destination, "v2", "latest")):
    for name in files:
        if name.endswith(".json"):
            path = os.path.join(directory, name)
            entry = json.load(open(path))
            entry["atMs"] = now
            json.dump(entry, open(path, "w"), separators=(",", ":"))
for directory, dirs, files in os.walk(destination):
    os.chmod(directory, 0o777)
    for name in files:
        os.chmod(os.path.join(directory, name), 0o666)
