#!/usr/bin/env bash
# Build the BasalDB container and deploy basald to Google Cloud Run.
#
# Usage:
#   PROJECT=my-project REGION=us-central1 ./deploy/cloudrun.sh
#
# Notes:
#   - Cloud Run services have no persistent disk; the .bsl container lives in
#     /tmp and is ephemeral across restarts. For durable storage, back the
#     file with a startup download/upload to Cloud Storage or run basald on
#     GCE/Cloud Run with a mounted volume.
#   - Cloud Run injects $PORT; basald honors it automatically.
set -euo pipefail

PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
REGION="${REGION:-us-central1}"
SERVICE="${SERVICE:-basald}"
IMAGE="${IMAGE:-gcr.io/${PROJECT}/basald}"

if [[ -z "${PROJECT}" || "${PROJECT}" == "(unset)" ]]; then
  echo "cloudrun.sh: set PROJECT=<gcp project id>" >&2
  exit 1
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

echo ">>> building ${IMAGE}"
gcloud builds submit "${ROOT}" --tag "${IMAGE}" --project "${PROJECT}"

echo ">>> deploying ${SERVICE} to Cloud Run (${REGION})"
gcloud run deploy "${SERVICE}" \
  --image "${IMAGE}" \
  --region "${REGION}" \
  --project "${PROJECT}" \
  --port 8080 \
  --memory 512Mi \
  --cpu 1 \
  --min-instances 0 \
  --max-instances 10 \
  --args "-file,/tmp/basal.bsl,-sync" \
  --allow-unauthenticated

echo ">>> done: $(gcloud run services describe "${SERVICE}" --region "${REGION}" --project "${PROJECT}" --format='value(status.url)')"
