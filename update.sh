#!/usr/bin/env bash
# ARTEX updater: 1) Docker (pull and recreate the image)  2) Build locally (rebuild the binary)
# Paired with install.sh: install performs the initial setup; update upgrades to a new version.
# No manual database migration is needed: artex reapplies schema.sql idempotently at startup
# (including ADD COLUMN / CREATE INDEX IF NOT EXISTS). Data (pgdata volume, ./data, ./skills)
# is preserved.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

# ── Optional: sync the repository to the latest code ──────────────────
sync_repo(){
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "Not a Git working copy; skipping git pull"; return; }
  [ "$(ask 'Pull the latest code (git pull --ff-only)? (y/n)' y)" = y ] || return
  if ! git pull --ff-only; then
    warn "git pull could not fast-forward (local changes or diverged branches); resolve manually and retry. Using the current code for now."
  fi
}

# ── 1) Docker update ─────────────────────────────
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "Docker / Docker Compose not found. Run ./install.sh to install first."
  [ -f .env ] || die ".env not found. Run ./install.sh to complete the initial setup."

  # Optional: upgrade to a specific version tag (defaults to ARTEX_TAG in .env, or latest).
  local tag; tag="$(ask 'Target image tag (press Enter to keep .env / latest)' '')"
  if [ -n "$tag" ]; then
    if grep -q '^ARTEX_TAG=' .env; then
      sed -i.bak "s|^ARTEX_TAG=.*|ARTEX_TAG=${tag}|" .env && rm -f .env.bak
    else
      printf '\nARTEX_TAG=%s\n' "$tag" >> .env
    fi
    ok "Set ARTEX_TAG to ${tag}"
  fi

  # Update only artex: postgres is pinned to 16-alpine and does not need upgrading.
  # Recreating it wastes bandwidth and major-version changes could cause incompatibilities.
  # artex declares depends_on postgres, so compose starts postgres if needed and leaves an
  # already-running instance untouched.
  info "Pulling the new image (artex only)…"
  docker compose pull artex
  info "Recreating and starting (artex migrates the schema at startup)…"
  docker compose up -d artex
  ok "Update complete → http://localhost:8787"
  info "View logs: docker compose logs -f artex"
  info "Remove old images (optional): docker image prune -f"
}

# ── 2) Build locally ─────────────────────────────
update_local(){
  command -v go >/dev/null 2>&1 || die "Go (>=1.26) not found: https://go.dev/dl/"
  [ -f config.json ] || warn "config.json not found; use ./install.sh for the initial setup"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "Rebuilding frontend static assets…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "Rebuilding the single binary with the frontend embedded…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm not found: building the backend without the embedded frontend (run the frontend separately with npm run dev)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "Build complete → ./artex"
  warn "Restart the running artex process to apply the update (the schema is migrated at startup)"
}

echo "=============================="
echo "  ARTEX Updater"
echo "  1) Docker update (pull and recreate image)"
echo "  2) Local update (rebuild with Go)"
echo "=============================="
case "$(ask 'Select an option' 1)" in
  1) sync_repo; update_docker ;;
  2) sync_repo; update_local ;;
  *) die "Invalid selection" ;;
esac
