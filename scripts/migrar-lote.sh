#!/bin/zsh
# Ejecuta un lote de migración Dropbox -> Internxt sin intervención.
# Pensado para launchd (ver launchd/com.internxt.dropbox2internxt.plist).
# Lee la configuración de .env y escribe el log en migracion/log/.

set -u
cd "$(dirname "$0")/.." || exit 1

if [[ ! -f .env ]]; then
  echo "Falta .env (copia .env.example)"; exit 1
fi
set -a; source .env; set +a

mkdir -p migracion/log bin
LOG="migracion/log/$(date +%Y-%m-%d).log"

# Compila solo si el binario no existe o el código es más nuevo.
if [[ ! -x bin/dropbox2internxt ]] || [[ -n "$(find cmd migrate webdav -newer bin/dropbox2internxt -name '*.go' 2>/dev/null)" ]]; then
  /usr/local/go/bin/go build -o bin/dropbox2internxt ./cmd/dropbox2internxt 2>>"$LOG" \
    || go build -o bin/dropbox2internxt ./cmd/dropbox2internxt 2>>"$LOG" || exit 1
fi

{
  echo "===== $(date '+%Y-%m-%d %H:%M:%S') inicio de lote"
  bin/dropbox2internxt run -yes \
    -src "${DROPBOX_DIR:-$HOME/Dropbox}" \
    -dst "${MIGRATE_DST:-/Dropbox}" \
    -state migracion/diario.jsonl \
    -batch-files "${MIGRATE_BATCH_FILES:-200}" \
    -batch-bytes "${MIGRATE_BATCH_BYTES:-2G}" \
    -verify "${MIGRATE_VERIFY:-hash}" \
    -parallel "${MIGRATE_PARALLEL:-2}"
  echo "===== fin de lote (exit $?)"
} >>"$LOG" 2>&1
