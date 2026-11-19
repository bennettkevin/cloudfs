# CloudFS

A self-hostable file sync service: keep a folder in sync across your devices, with a backend you own. The same system runs locally (Docker Compose + MinIO) or in AWS (S3, RDS, ECS), provisioned with Terraform.

**Status:** Sprint 0 — design and research. No runnable code yet.

## Docs

- [Design doc](docs/design.md): living document, updated at each sprint review
- [Architecture Decision Records](docs/adr/): why things are the way they are
- [Research spikes](docs/research/): findings that feed the decisions

## Self-hosting note

CloudFS is built as a personal, single-user tool. If you run it for other people, you're responsible for your own privacy policy and terms of service for those users.

## License

[MIT](LICENSE)
