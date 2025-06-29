# IndexerSync

Sync Prowlarr indexer privacy settings into *rr stack, currently only supports Sonarr/Radarr.

## Getting Started

1. Copy **.env.example** → **.env** and fill in your credentials.
2. Build & run with Docker Compose:
   ```
   docker compose up --build
   ```

## Files

- `IndexerSync.go` — main Go code  
- `Dockerfile` — multi-stage build  
- `docker-compose.yml` — orchestrate build & run  
- `.env.example` — sample env-vars  
