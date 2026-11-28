#!/bin/bash
# Points the OpenAI Realtime client of OpenWhispr at the local stt-router.
# An OpenWhispr update replaces app.asar and removes the patch: run this again.
#   sudo openwhispr-patch.sh            apply
#   sudo openwhispr-patch.sh --restore  put the original file back
set -euo pipefail
ASAR="${OPENWHISPR_ASAR:-/opt/OpenWhispr/resources/app.asar}"
if [ "${1:-}" = "--restore" ]; then
  cp -p "$ASAR.orig" "$ASAR"
  echo "restored $ASAR"
  exit 0
fi
exec python3 - "$ASAR" <<'PY'
import os, shutil, sys
path = sys.argv[1]
old = b"wss://api.openai.com/v1/realtime?intent=transcription"
new = b"ws://127.0.0.1:8330/v1/realtime?intent=transcription&"
assert len(old) == len(new)  # same length keeps every offset in the archive valid
data = open(path, "rb").read()
if data.count(old) == 0 and data.count(new) == 1:
    sys.exit("already patched: " + path)
if data.count(old) != 1:
    sys.exit(f"expected the OpenAI realtime URL 1 time, found {data.count(old)}: this OpenWhispr version needs a new patch")
shutil.copy2(path, path + ".orig")
tmp = path + ".tmp"
with open(tmp, "wb") as f:
    f.write(data.replace(old, new))
shutil.copymode(path, tmp)
os.replace(tmp, path)
print("patched", path, "- restart OpenWhispr")
PY
