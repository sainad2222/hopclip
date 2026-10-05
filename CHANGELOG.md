# Changelog

All notable changes to this project are documented here.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Live clipboard sync between signed-in devices over Server-Sent Events, with history.
- File sharing with streamed uploads, progress, cancel, drag and drop, paste-to-upload, image previews and resumable downloads.
- Per-file size limit, per-user quota, and optional retention for clips and files.
- Username/password accounts with argon2id, sliding sessions, device list with online status, rename and remote sign-out.
- Admin UI and `hopclip user` CLI for user management.
- Docker image (linux/amd64, linux/arm64) and Compose profiles for Cloudflare Tunnel and Caddy.

[Unreleased]: https://github.com/sainad2222/hopclip/commits/main
