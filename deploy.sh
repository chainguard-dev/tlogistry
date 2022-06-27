#!/usr/bin/env bash

set -e
set -o pipefail

img=$(ko publish ./)

gcloud run deploy tlogistry \
  --image=${img} \
  --allow-unauthenticated \
  --region=us-east4

# TODO(jason): run this as a separate SA with minimal permissions.