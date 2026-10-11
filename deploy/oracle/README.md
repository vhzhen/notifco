# Deploying notifco on an Oracle Cloud Always Free VM

Target: **$0/month, forever.** Everything self-hosted on one small ARM VM, no
managed services, nothing billable.

## Why this and not a PaaS

Checked October 2026:

| Option | Cost | Verdict |
|---|---|---|
| **Oracle Always Free** | $0 | What this guide uses |
| Fly.io | ~$2.19/mo smallest always-on app, plus a database | Free allowance is gone |
| Render free tier | $0 | **Spins down when idle** — fatal for a poller |
| Railway | metered + plan fee | No free tier |
| Neon free Postgres | $0 | 100 CU-hours/mo and scale-to-zero: a bot querying every 60s never idles and runs dry mid-month |

A watcher that sleeps is worse than no watcher, because users believe they are
covered. That rules out anything that idles the process.

> **Heads up on Oracle.** On 15 June 2026 Oracle halved the Always Free Ampere
> allowance from 4 OCPU / 24 GB to **2 OCPU / 12 GB**, with no announcement, and
> began enforcing it in August. Still free, still roughly 12× more than notifco
> needs, but expect that kind of behaviour. Oracle also reclaims idle free
> instances. If it becomes annoying, a ~$5/month VPS removes every caveat.

notifco's actual footprint: the Go binary idles around 20–40 MB, Postgres ~150 MB,
Redis is capped at 128 MB. Comfortable inside a 1 OCPU / 6 GB slice.

## 1. Create the VM

In the Oracle Cloud console:

1. **Compute → Instances → Create instance**
2. Image: **Canonical Ubuntu 24.04**
3. Shape: **Ampere / VM.Standard.A1.Flex**, 1 OCPU, 6 GB RAM
   (stays within the 2 OCPU / 12 GB free allowance and leaves room for a second VM)
4. Add your SSH public key
5. Create, and note the public IP

Capacity errors on ARM are common; retry in a different availability domain.

No inbound ports are needed. A Discord bot connects **outbound** over a
WebSocket, so leave the security list closed except for SSH. Nothing about
notifco should be internet-reachable.

## 2. Install Docker

```sh
ssh ubuntu@YOUR_IP

sudo apt-get update && sudo apt-get upgrade -y
sudo apt-get install -y ca-certificates curl git
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
  -o /etc/apt/keyrings/docker.asc
sudo chmod a+r /etc/apt/keyrings/docker.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] \
https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo $VERSION_CODENAME) stable" \
  | sudo tee /etc/apt/sources.list.d/docker.list > /dev/null
sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin

sudo usermod -aG docker ubuntu
newgrp docker
```

Oracle's Ubuntu images ship a restrictive iptables config. Docker's bridge
networking needs this, otherwise containers cannot reach each other:

```sh
sudo iptables -I INPUT 1 -i docker0 -j ACCEPT
sudo netfilter-persistent save   # apt-get install -y iptables-persistent if missing
```

## 3. Deploy

```sh
git clone https://github.com/vhzhen/notifco.git
cd notifco
cp .env.sample .env
nano .env
```

Set at minimum:

```sh
DISCORD_TOKEN=...          # Discord Developer Portal -> Bot -> Reset Token
DISCORD_APP_ID=...         # Developer Portal -> General Information -> Application ID
POSTGRES_PASSWORD=...      # anything long and random
UW_API_KEY=...             # optional; empty means fixtures instead of live data
NOTIFCO_SOURCE=uwapi       # omit to auto-detect from UW_API_KEY
```

Then:

```sh
docker compose up -d --build
docker compose logs -f notifco
```

Expect to see `database ready`, `redis ready`, `slash commands registered`, and
`metrics listening`.

## 4. Verify

```sh
curl -s localhost:9090/healthz                       # -> ok
curl -s localhost:9090/metrics | grep notifco_ | head
docker compose ps                                     # all healthy
```

In Discord: `/search CS 341`, then `/watch CS 341 LEC 002`.

## 5. Survive reboots

`restart: unless-stopped` handles container restarts, but Docker itself must
come up on boot:

```sh
sudo systemctl enable docker
```

Optionally run the stack as a unit so it is managed like any other service:

```sh
sudo tee /etc/systemd/system/notifco.service > /dev/null <<'EOF'
[Unit]
Description=notifco
Requires=docker.service
After=docker.service network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
WorkingDirectory=/home/ubuntu/notifco
ExecStart=/usr/bin/docker compose up -d
ExecStop=/usr/bin/docker compose down
User=ubuntu

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now notifco
```

## 6. Back up

Only Postgres holds anything irreplaceable (users, subscriptions, notification
log). Redis is a cache and the catalog can be re-fetched.

```sh
mkdir -p ~/backups
cat > ~/backup-notifco.sh <<'EOF'
#!/bin/sh
set -eu
cd /home/ubuntu/notifco
docker compose exec -T postgres pg_dump -U notifco notifco \
  | gzip > "/home/ubuntu/backups/notifco-$(date +%F).sql.gz"
find /home/ubuntu/backups -name 'notifco-*.sql.gz' -mtime +14 -delete
EOF
chmod +x ~/backup-notifco.sh
( crontab -l 2>/dev/null; echo "0 4 * * * /home/ubuntu/backup-notifco.sh" ) | crontab -
```

Restore:

```sh
gunzip -c ~/backups/notifco-YYYY-MM-DD.sql.gz \
  | docker compose exec -T postgres psql -U notifco -d notifco
```

## 7. Upgrade

```sh
cd ~/notifco && git pull && docker compose up -d --build
```

Migrations are idempotent and run automatically on boot.

## Operating notes

**Rate limit.** Default is the documented 120 req/min even though ~409/min was
observed without a 429. The docs call the limit experimental and send no
rate-limit headers, so the headroom is margin, not budget. Raising
`NOTIFCO_RATE_LIMIT` increases how many distinct courses a 60s interval supports
(~120 at the default), at the cost of being a worse citizen.

**Scaling past one VM.** The differ and notifier are already safe to run
concurrently — the Lua CAS and `UNIQUE(subscription_id, transition_id)` handle
it. The poller is not: two pollers would duplicate upstream requests, wasting
budget rather than breaking correctness. Splitting would need a distributed lock
on the poll schedule.

**Security.** Postgres, Redis, and `/metrics` are all bound to `127.0.0.1`.
`/metrics` is unauthenticated by design and must not be exposed; tunnel over SSH
if you want to see it:

```sh
ssh -L 9090:localhost:9090 ubuntu@YOUR_IP
```

Keep `.env` out of git — it is already in `.gitignore`.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `redis ping` fails from the app container | Oracle iptables; apply the `docker0` ACCEPT rule in step 2 |
| Slash commands never appear | Global commands take up to an hour. Set `DISCORD_GUILD_ID` for instant registration while testing |
| `NOTIFCO_SOURCE=uwapi but UW_API_KEY is empty` | Set the key, or unset `NOTIFCO_SOURCE` to fall back to fixtures |
| Bot online but no alerts | Alerts fire only on a full → open edge. An already-open section never notifies until it fills and frees again |
| 401 from the UW API | Key wrong or revoked; re-register per the README |
| Instance disappeared | Oracle reclaims idle free instances. This is the tradeoff for $0 |
