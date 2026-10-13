#!/usr/bin/env bash
# Builds installable release packages:
#   dist/camera-network-monitor-<version>-linux-<arch>.tar.gz
# Each package contains bin/cnm (static), bin/go2rtc (bundled, MIT), the
# systemd unit, install/uninstall scripts and the documentation.
#
#   scripts/build-release.sh [version] [--no-go2rtc] [--arch "amd64 arm64 arm"]
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-$(git describe --tags --always 2>/dev/null || echo 1.0.0)}"
case "$VERSION" in --*) VERSION="1.0.0";; esac
GO2RTC_VERSION="${GO2RTC_VERSION:-v1.9.14}"
ARCHES="amd64 arm64 arm"
FETCH_GO2RTC=1
args=("$@")
for ((i=0; i<${#args[@]}; i++)); do
  case "${args[$i]}" in
    --no-go2rtc) FETCH_GO2RTC=0 ;;
    --arch) ARCHES="${args[$((i+1))]}"; i=$((i+1)) ;;
  esac
done

echo "== tests"
go vet ./...
go test ./... >/dev/null

PY=""
for c in python3 python; do
  if command -v "$c" >/dev/null 2>&1 && "$c" -c 'import tarfile' >/dev/null 2>&1; then PY="$c"; break; fi
done
[ -n "$PY" ] || { echo "python3 wird zum Packen benötigt" >&2; exit 1; }
# pack writes the tarball with explicit permissions (works on Windows builds too)
pack() {
  "$PY" - "$1" <<'PYEOF'
import os, sys, tarfile
name = sys.argv[1]
root = os.path.join("dist", name)
def mode(rel, is_dir):
    if is_dir:
        return 0o755
    if rel.startswith("bin/") or rel.endswith(".sh"):
        return 0o755
    return 0o644
with tarfile.open(os.path.join("dist", name + ".tar.gz"), "w:gz") as tar:
    for base, dirs, files in os.walk(root):
        dirs.sort()
        for entry in [base] + sorted(os.path.join(base, f) for f in files):
            rel = os.path.relpath(entry, root).replace(os.sep, "/")
            ti = tar.gettarinfo(entry, arcname=name if rel == "." else name + "/" + rel)
            ti.mode = mode(rel, os.path.isdir(entry))
            ti.uid = ti.gid = 0
            ti.uname = ti.gname = "root"
            if ti.isdir():
                tar.addfile(ti)
            else:
                with open(entry, "rb") as fh:
                    tar.addfile(ti, fh)
PYEOF
}

mkdir -p dist .cache
for ARCH in $ARCHES; do
  NAME="camera-network-monitor-${VERSION}-linux-${ARCH}"
  OUT="dist/$NAME"
  rm -rf "$OUT"
  mkdir -p "$OUT/bin" "$OUT/deploy" "$OUT/docs"
  echo "== build $ARCH"
  GOARM=""
  [ "$ARCH" = "arm" ] && GOARM=7
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" GOARM="$GOARM" go build -trimpath \
    -ldflags "-s -w -X cnm/internal/buildinfo.Version=${VERSION}" -o "$OUT/bin/cnm" ./cmd/cnm
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" GOARM="$GOARM" go build -trimpath \
    -ldflags "-s -w" -o "$OUT/bin/cnm-sim" ./cmd/cnm-sim
  if [ "$FETCH_GO2RTC" -eq 1 ]; then
    G="go2rtc_linux_${ARCH}"
    [ "$ARCH" = "arm" ] && G="go2rtc_linux_arm"
    CACHE=".cache/${GO2RTC_VERSION}_${G}"
    if [ ! -f "$CACHE" ]; then
      echo "   download go2rtc ${GO2RTC_VERSION} ($G)"
      curl -fsSL -o "$CACHE" "https://github.com/AlexxIT/go2rtc/releases/download/${GO2RTC_VERSION}/${G}"
    fi
    install -m 0755 "$CACHE" "$OUT/bin/go2rtc"
  fi
  install -m 0644 deploy/camera-network-monitor.service deploy/camera-network-monitor-display.service \
    deploy/camera-network-monitor-display.pam "$OUT/deploy/"
  install -m 0755 deploy/install.sh "$OUT/install.sh"
  install -m 0755 deploy/setup.sh "$OUT/setup.sh"
  install -m 0755 deploy/uninstall.sh "$OUT/uninstall.sh"
  cp docs/*.md "$OUT/docs/"
  cp README.md CHANGELOG.md LICENSE THIRD_PARTY_NOTICES.md "$OUT/"
  echo "$VERSION" > "$OUT/VERSION"
  pack "$NAME"
  (cd dist && sha256sum "$NAME.tar.gz" > "$NAME.tar.gz.sha256")
  echo "   dist/$NAME.tar.gz"
  NAMES="${NAMES:-} $NAME"
done

# ---- one self-extracting installer for all architectures ------------------------------------
RUN="dist/camera-network-monitor-${VERSION}-installer.run"
echo "== installer $RUN"
"$PY" - "$RUN" "$VERSION" $NAMES <<'PYEOF'
import hashlib, io, os, sys, tarfile
out, version, names = sys.argv[1], sys.argv[2], sys.argv[3:]
def mode(rel, is_dir):
    if is_dir or rel.split("/", 1)[-1].startswith("bin/") or rel.endswith(".sh"):
        return 0o755
    return 0o644
buf = io.BytesIO()
with tarfile.open(fileobj=buf, mode="w:gz") as tar:
    for name in names:
        root = os.path.join("dist", name)
        for base, dirs, files in os.walk(root):
            dirs.sort()
            for entry in [base] + sorted(os.path.join(base, f) for f in files):
                rel = os.path.relpath(entry, "dist").replace(os.sep, "/")
                ti = tar.gettarinfo(entry, arcname=rel)
                ti.mode = mode(rel, os.path.isdir(entry))
                ti.uid = ti.gid = 0
                ti.uname = ti.gname = "root"
                if ti.isdir():
                    tar.addfile(ti)
                else:
                    with open(entry, "rb") as fh:
                        tar.addfile(ti, fh)
payload = buf.getvalue()
with open("deploy/installer-header.sh", encoding="utf-8") as fh:
    header = fh.read()
header = header.replace("@VERSION@", version).replace("@PAYLOAD_SHA256@", hashlib.sha256(payload).hexdigest())
assert header.endswith("__CNM_PAYLOAD__\n"), "installer header must end with the payload marker line"
with open(out, "wb") as fh:
    fh.write(header.encode("utf-8"))
    fh.write(payload)
os.chmod(out, 0o755)
PYEOF
(cd dist && sha256sum "$(basename "$RUN")" > "$(basename "$RUN").sha256")
echo "   $RUN ($(du -h "$RUN" | cut -f1))"

# ---- one archive with everything needed for the installation ---------------------------------
#   camera-network-monitor-<v>/setup.sh (picks the architecture), INSTALL.txt,
#   linux-amd64/ linux-arm64/ linux-arm/, docs/, SHA256SUMS
ALL="camera-network-monitor-${VERSION}"
echo "== archive dist/$ALL.tar.gz"
"$PY" - "$ALL" "$VERSION" $NAMES <<'PYEOF'
import glob, hashlib, io, os, sys, tarfile, time
top, version, names = sys.argv[1], sys.argv[2], sys.argv[3:]
entries = {}  # arcname (relative to top) -> (bytes, mode)
def put(rel, data, mode):
    entries[rel] = (data, mode)
def read(path):
    with open(path, "rb") as fh:
        return fh.read()
for name in names:
    arch = "-".join(name.split("-")[-2:])          # linux-amd64
    root = os.path.join("dist", name)
    for base, dirs, files in os.walk(root):
        for f in files:
            path = os.path.join(base, f)
            rel = os.path.relpath(path, root).replace(os.sep, "/")
            exe = rel.startswith("bin/") or rel.endswith(".sh")
            put(arch + "/" + rel, read(path), 0o755 if exe else 0o644)
put("setup.sh", read("deploy/universal-setup.sh"), 0o755)
put("INSTALL.txt", read("deploy/INSTALL.txt").replace(b"@VERSION@", version.encode()), 0o644)
for f in ("README.md", "CHANGELOG.md", "LICENSE", "THIRD_PARTY_NOTICES.md"):
    put(f, read(f), 0o644)
put("VERSION", (version + "\n").encode(), 0o644)
for f in sorted(glob.glob("docs/*.md")):
    put("docs/" + os.path.basename(f), read(f), 0o644)
sums = "".join("%s  %s\n" % (hashlib.sha256(entries[r][0]).hexdigest(), r) for r in sorted(entries))
put("SHA256SUMS", sums.encode(), 0o644)
now = int(time.time())
dirs = set()
for rel in entries:
    parts = rel.split("/")[:-1]
    for i in range(1, len(parts) + 1):
        dirs.add("/".join(parts[:i]))
def info(name, mode, size=0, is_dir=False):
    ti = tarfile.TarInfo(top + ("/" + name if name else ""))
    ti.mode, ti.size, ti.mtime = mode, size, now
    ti.uid = ti.gid = 0
    ti.uname = ti.gname = "root"
    if is_dir:
        ti.type = tarfile.DIRTYPE
    return ti
out = os.path.join("dist", top + ".tar.gz")
with tarfile.open(out, "w:gz") as tar:
    tar.addfile(info("", 0o755, is_dir=True))
    for d in sorted(dirs):
        tar.addfile(info(d, 0o755, is_dir=True))
    for rel in sorted(entries):
        data, mode = entries[rel]
        tar.addfile(info(rel, mode, len(data)), io.BytesIO(data))
PYEOF
(cd dist && sha256sum "$ALL.tar.gz" > "$ALL.tar.gz.sha256")
echo "   dist/$ALL.tar.gz ($(du -h "dist/$ALL.tar.gz" | cut -f1))"
