#!/usr/bin/env bash
# ARTEX installer: 1) Docker deployment  2) Build and run locally
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }
rand(){ head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24; }

# ── Docker detection / automatic installation ────
ensure_docker(){
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    ok "Docker and Docker Compose detected"; return
  fi
  warn "Docker / Docker Compose not found"
  case "$(uname -s)" in
    Linux)
      if [ "$(ask 'Install Docker automatically? (y/n)' y)" = y ]; then
        curl -fsSL https://get.docker.com | sh
        sudo usermod -aG docker "$USER" || true
        ok "Docker installed (log out and back in to use Docker without sudo)"
      else
        die "Please install Docker manually and try again"
      fi ;;
    Darwin) die "Install Docker Desktop on macOS: https://www.docker.com/products/docker-desktop/" ;;
    *)      die "Please install Docker manually and try again" ;;
  esac
}

# ── 1) Docker deployment ─────────────────────────
install_docker(){
  ensure_docker
  if [ ! -f .env ]; then
    cp .env.example .env 2>/dev/null || true
    local pw key
    pw="$(ask 'Postgres password (press Enter to generate randomly)' "$(rand)")"
    key="$(ask 'ANTHROPIC_API_KEY (optional; configure later in the UI)' '')"
    sed -i.bak "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=${pw}|" .env
    sed -i.bak "s|^ANTHROPIC_API_KEY=.*|ANTHROPIC_API_KEY=${key}|" .env
    rm -f .env.bak
    ok "Generated .env (POSTGRES_PASSWORD is set)"
  else
    info "Using existing .env"
  fi
  info "Pulling images and starting services…"
  docker compose pull || true
  docker compose up -d
  ok "Started → http://localhost:8787"
  info "View logs: docker compose logs -f artex"
}

# ── 2) Build and run locally ─────────────────────
install_local(){
  echo "Database setup:"
  echo "  1) Connect to an existing PostgreSQL server"
  echo "  2) Start PostgreSQL with Docker (requires Docker)"
  case "$(ask 'Select an option' 1)" in
    2)
      ensure_docker
      local pw; pw="$(ask 'Postgres password (press Enter to generate randomly)' "$(rand)")"
      docker run -d --name artex-pg -p 5432:5432 \
        -e POSTGRES_USER=artex -e POSTGRES_PASSWORD="$pw" -e POSTGRES_DB=artex \
        -v artex-pg:/var/lib/postgresql/data postgres:16-alpine
      DB_HOST=127.0.0.1 DB_PORT=5432 DB_USER=artex DB_PASS="$pw" DB_NAME=artex DB_SSL=disable ;;
    *)
      DB_HOST="$(ask 'Database host' 127.0.0.1)"
      DB_PORT="$(ask 'Port' 5432)"
      DB_USER="$(ask 'Username' artex)"
      DB_PASS="$(ask 'Password' '')"
      DB_NAME="$(ask 'Database name' artex)"
      DB_SSL="$(ask 'sslmode (disable/require)' disable)" ;;
  esac

  # Generate config.json.
  cat > config.json <<JSON
{
  "database": {
    "host": "${DB_HOST}",
    "port": ${DB_PORT},
    "user": "${DB_USER}",
    "password": "${DB_PASS}",
    "dbname": "${DB_NAME}",
    "sslmode": "${DB_SSL}"
  }
}
JSON
  ok "Generated config.json"

  # Check for Go.
  command -v go >/dev/null 2>&1 || die "Go not found. Install Go (>=1.26): https://go.dev/dl/"
  ok "Go: $(go version)"

  # Node.js is required to generate the static assets for the embedded frontend.
  if command -v npm >/dev/null 2>&1; then
    info "Building frontend static assets…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "Building the single binary with the frontend embedded…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm not found: building the backend without the embedded frontend (run the frontend separately with npm run dev)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "Build complete → ./artex"

  info "Starting… (press Ctrl-C to exit)"
  ./artex
}

echo "=============================="
echo "  ARTEX Installer"
echo "  1) Docker deployment"
echo "  2) Run locally (build with Go)"
echo "=============================="
case "$(ask 'Select an option' 1)" in
  1) install_docker ;;
  2) install_local ;;
  *) die "Invalid selection" ;;
esac
