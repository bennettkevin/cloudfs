# Spike 07: presigned upload checks

A small Go program that checks how an S3 store treats presigned uploads and downloads: whether it enforces the signed SHA-256 checksum, length, expiry, host and range, and whether it honours `If-None-Match`. Results and their meaning are in [docs/research/07-uploads.md](../../docs/research/07-uploads.md), section 11; decisions in [ADR-0015](../../docs/adr/0015-direct-presigned-chunk-transfer.md).

This is spike code, not part of CloudFS. It's the starting point for the Garage contract tests in phase 1.

## Run against gofakes3 (no setup)

```sh
go run .
```

gofakes3 runs inside the program. It enforces almost none of these checks, which is the point of the comparison.

## Run against Garage

Start a throwaway single-node Garage (data is lost when the container stops):

```sh
cat > garage.toml <<TOML
metadata_dir = "/var/lib/garage/meta"
data_dir = "/var/lib/garage/data"
db_engine = "sqlite"
replication_factor = 1
rpc_bind_addr = "[::]:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "$(openssl rand -hex 32)"

[s3_api]
s3_region = "garage"
api_bind_addr = "[::]:3900"
root_domain = ".s3.garage.localhost"
TOML

export GARAGE_DEFAULT_ACCESS_KEY="GK$(openssl rand -hex 12)"
export GARAGE_DEFAULT_SECRET_KEY="$(openssl rand -hex 32)"
export GARAGE_DEFAULT_BUCKET=spike07

docker run -d --rm --name cloudfs-spike07-garage -p 127.0.0.1:3900:3900 \
  -v "$PWD/garage.toml:/etc/garage.toml:ro" \
  -e GARAGE_DEFAULT_ACCESS_KEY -e GARAGE_DEFAULT_SECRET_KEY -e GARAGE_DEFAULT_BUCKET \
  dxflrs/garage:v2.5.0 /garage server --single-node --default-bucket
```

Run the cases, then stop the container:

```sh
S3_ENDPOINT=http://localhost:3900 S3_REGION=garage S3_BUCKET=spike07 S3_PATH_STYLE=1 \
AWS_ACCESS_KEY_ID=$GARAGE_DEFAULT_ACCESS_KEY AWS_SECRET_ACCESS_KEY=$GARAGE_DEFAULT_SECRET_KEY \
go run .

docker stop cloudfs-spike07-garage
rm garage.toml
```

## Run against AWS S3 (phase 2)

Set `S3_ENDPOINT=` (empty), `S3_REGION`, `S3_BUCKET` (an existing test bucket) and credentials scoped to that bucket. The program deletes the objects it creates. Case 11 (different host) is skipped on non-local endpoints.
