#!/usr/bin/env bash
# On-demand container runtime for macOS: installs Colima + Docker CLI via
# Homebrew, runs them only while needed, and removes every trace afterwards.
#
#   scripts/runtime.sh up          # Docker only        (2 CPU / 4 GB VM)
#   scripts/runtime.sh up --k8s    # Docker + k3s + ingress-nginx (4 CPU / 6 GB VM)
#   scripts/runtime.sh stop        # stop the VM (keeps images and packages)
#   scripts/runtime.sh down        # delete VM, images, packages, config, caches
#   scripts/runtime.sh status
#
# VM sizes are caps, not reservations: an idle VM uses almost no CPU/RAM.
# kubectl lives in .venv (scripts/bootstrap-venv.sh), not in Homebrew.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PKGS=(colima lima docker docker-compose docker-buildx)
PLUGINS_DIR="$(brew --prefix 2>/dev/null || echo /opt/homebrew)/lib/docker/cli-plugins"
INGRESS_NGINX_VERSION="${INGRESS_NGINX_VERSION:-controller-v1.15.1}"

need_macos() {
  [ "$(uname -s)" = Darwin ] || { echo "runtime.sh manages Colima on macOS; on Linux install Docker Engine directly." >&2; exit 1; }
  command -v brew >/dev/null || { echo "Homebrew is required: https://brew.sh" >&2; exit 1; }
}

install_pkgs() {
  local missing=()
  for p in "${PKGS[@]}"; do brew list --formula "$p" >/dev/null 2>&1 || missing+=("$p"); done
  if [ ${#missing[@]} -gt 0 ]; then
    echo "==> installing ${missing[*]}"
    brew install "${missing[@]}"
  fi
  # Let the docker CLI find the compose/buildx plugins (merge, don't clobber).
  mkdir -p "$HOME/.docker"
  python3 - "$HOME/.docker/config.json" "$PLUGINS_DIR" <<'PY'
import json, os, sys
path, plugins = sys.argv[1], sys.argv[2]
cfg = json.load(open(path)) if os.path.exists(path) else {}
dirs = cfg.setdefault("cliPluginsExtraDirs", [])
if plugins not in dirs:
    dirs.append(plugins)
json.dump(cfg, open(path, "w"), indent=2)
PY
}

up() {
  need_macos
  local k8s=0
  [ "${1:-}" = "--k8s" ] && k8s=1
  install_pkgs
  if [ "$k8s" = 1 ]; then
    # Colima checks for kubectl on PATH before starting k3s; it lives in .venv.
    # shellcheck disable=SC1091
    source "$ROOT/.venv/bin/activate"
    command -v kubectl >/dev/null || { echo "kubectl missing: run scripts/bootstrap-venv.sh" >&2; exit 1; }
    echo "==> starting Colima with Kubernetes (4 CPU / 6 GB / 20 GB disk)"
    colima start --cpu 4 --memory 6 --disk 20 --vm-type vz --kubernetes
    kubectl wait --for=condition=Ready node --all --timeout=180s
    echo "==> installing ingress-nginx $INGRESS_NGINX_VERSION"
    kubectl apply -f "https://raw.githubusercontent.com/kubernetes/ingress-nginx/$INGRESS_NGINX_VERSION/deploy/static/provider/baremetal/deploy.yaml" >/dev/null
    kubectl -n ingress-nginx wait --for=condition=Available deploy/ingress-nginx-controller --timeout=300s
    echo "==> ready. Next: make k8s-deploy && make k8s-test"
  else
    echo "==> starting Colima (2 CPU / 4 GB / 20 GB disk)"
    colima start --cpu 2 --memory 4 --disk 20 --vm-type vz
    echo "==> ready. Next: docker compose up -d --build --wait && make compose-test"
  fi
  docker info --format 'docker {{.ServerVersion}} | {{.NCPU}} CPU | {{.OperatingSystem}}'
}

stop() {
  need_macos
  command -v colima >/dev/null && colima stop || true
}

down() {
  need_macos
  if command -v docker >/dev/null && docker info >/dev/null 2>&1; then
    (cd "$ROOT" && docker compose down -v --remove-orphans >/dev/null 2>&1) || true
  fi
  if command -v colima >/dev/null; then
    colima stop >/dev/null 2>&1 || true
    colima delete --force >/dev/null 2>&1 || true
  fi
  local installed=()
  for p in "${PKGS[@]}"; do brew list --formula "$p" >/dev/null 2>&1 && installed+=("$p"); done
  [ ${#installed[@]} -gt 0 ] && brew uninstall --force "${installed[@]}"
  rm -rf "$HOME/.colima" "$HOME/.lima"
  # Remove ~/.docker only if its config holds nothing beyond what this script
  # and Colima write (plugin dir, colima context, empty auths): no saved logins.
  if [ -f "$HOME/.docker/config.json" ] && python3 - "$HOME/.docker/config.json" <<'PY'
import json, sys
cfg = json.load(open(sys.argv[1]))
ours = {"cliPluginsExtraDirs", "currentContext"}
sys.exit(0 if set(cfg) - ours <= {"auths"} and not cfg.get("auths") and cfg.get("currentContext", "colima") == "colima" else 1)
PY
  then rm -rf "$HOME/.docker"; fi
  # Remove ~/.kube only if every context in it belongs to colima.
  if [ -f "$HOME/.kube/config" ] && ! grep -E "^[[:space:]]*(cluster|name|user):[[:space:]]*[^[:space:]]" "$HOME/.kube/config" | grep -v colima >/dev/null; then
    rm -rf "$HOME/.kube"
  fi
  # Drop Homebrew's cached downloads for these packages.
  local cache; cache="$(brew --cache)"
  for p in "${PKGS[@]}"; do rm -rf "$cache"/"$p"--* "$cache"/"$p"_bottle_manifest--* "$cache"/downloads/*--"$p"--* 2>/dev/null || true; done
  echo "==> removed Colima VM, images, packages, config and caches"
}

status() {
  for p in "${PKGS[@]}"; do printf '%-15s %s\n' "$p" "$(brew list --versions "$p" 2>/dev/null || echo 'not installed')"; done
  if command -v colima >/dev/null; then
    if colima status >/dev/null 2>&1; then echo "colima VM      running"; else echo "colima VM      stopped"; fi
  fi
  for d in "$HOME/.colima" "$HOME/.lima" "$HOME/.docker" "$HOME/.kube"; do [ -e "$d" ] && du -sh "$d"; done
  return 0
}

case "${1:-}" in
  up) shift; up "$@" ;;
  stop) stop ;;
  down) down ;;
  status) status ;;
  *) sed -n '2,13p' "$0"; exit 2 ;;
esac
