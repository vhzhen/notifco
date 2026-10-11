# Deploying notifco on Google Cloud (always free)

Target: **$0/month, indefinitely**, on Compute Engine's always-free `e2-micro`.

Verified against Google's own free-tier documentation on 2026-10-11.

## What "free" means here, exactly

| Allowance | Limit |
|---|---|
| VM | **1 non-preemptible `e2-micro` per month** |
| Eligible regions | **`us-west1`** (Oregon), **`us-central1`** (Iowa), **`us-east1`** (South Carolina) — nowhere else |
| Disk | **30 GB-months standard persistent disk** (`pd-standard`, not balanced or SSD) |
| Egress | **1 GB/month** from North America |

Three things will quietly cost you money if you get them wrong:

1. **Region.** An `e2-micro` in `northamerica-northeast2` (Toronto, the closest one to Waterloo) is **not free**. Use `us-central1` — ~30 ms further away, which is irrelevant for a 60-second poll loop.
2. **Disk type.** The console defaults to **Balanced**. That is billable. You must pick **Standard persistent disk**.
3. **The free VM is counted by hours, not instances.** One VM running 24/7 consumes the whole monthly allowance. A second one is billable.

`e2-micro` has **1 GB of RAM**, which is the real constraint — hence `docker-compose.gcp.yml` and the swap step below. notifco itself is small; Postgres is the hungry one.

Egress is fine: notifco's outbound traffic is HTTP request headers and Discord WebSocket frames. The 7.1 MB course catalog is a *response*, and **ingress is free**. Expect a few tens of MB per month against the 1 GB allowance.

---

## 1. Create the VM

Easiest from Cloud Shell (the `>_` icon in the console toolbar) — no local SDK needed:

```sh
gcloud config set project YOUR_PROJECT_ID

gcloud compute instances create notifco \
  --zone=us-central1-a \
  --machine-type=e2-micro \
  --image-family=ubuntu-2404-lts-amd64 \
  --image-project=ubuntu-os-cloud \
  --boot-disk-size=30GB \
  --boot-disk-type=pd-standard \
  --metadata=enable-oslogin=TRUE
```

Every flag matters: `e2-micro` + `us-central1-a` + `pd-standard` + `30GB` is exactly the free envelope.

Or via the console: **Compute Engine → VM instances → Create instance**, Region `us-central1`, Series **E2**, Machine type **e2-micro**, Boot disk → **Change** → Ubuntu 24.04 LTS, size **30 GB**, type **Standard persistent disk**.

**Do not add any firewall rules.** Leave "Allow HTTP traffic" unchecked. notifco is a Discord *gateway* bot: it dials out over a WebSocket and nothing ever connects to it. The only inbound you need is SSH, which GCP permits by default.

## 2. Connect

```sh
gcloud compute ssh notifco --zone=us-central1-a
```

(Or the **SSH** button next to the instance in the console.)

## 3. Add swap — do not skip this

1 GB of RAM with Postgres, Redis and Go in it has no headroom. Without swap, a
transient spike gets something OOM-killed; with it, the box just slows down.

```sh
sudo fallocate -l 2G /swapfile
sudo chmod 600 /swapfile
sudo mkswap /swapfile
sudo swapon /swapfile
echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab

# Prefer RAM, use swap only under real pressure.
echo 'vm.swappiness=10' | sudo tee /etc/sysctl.d/99-swap.conf
sudo sysctl -p /etc/sysctl.d/99-swap.conf

free -h   # should show 2.0Gi of swap
```

The 2 GB file sits on the free 30 GB disk, so it costs nothing.

## 4. Install Docker

```sh
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
sudo apt-get install -y docker-ce docker-ce-cli containerd.io \
  docker-buildx-plugin docker-compose-plugin

sudo usermod -aG docker "$USER"
newgrp docker
sudo systemctl enable docker
```

GCP's Ubuntu images need no iptables workaround — that one is Oracle-specific.

## 5. Deploy

```sh
git clone https://github.com/vhzhen/notifco.git
cd notifco
cp .env.sample .env
nano .env
```

Fill in:

```sh
DISCORD_TOKEN=...            # Developer Portal -> Bot -> Reset Token
DISCORD_APP_ID=...           # Developer Portal -> General Information
DISCORD_GUILD_ID=...         # your server id; omit for global registration
UW_API_KEY=...               # leave empty to run on fixtures
NOTIFCO_SOURCE=uwapi
POSTGRES_PASSWORD=...        # anything long and random
```

Then start it with **both** compose files, base first:

```sh
docker compose -f docker-compose.yml -f docker-compose.gcp.yml up -d --build
docker compose logs -f notifco
```

> The first build compiles Go on a shared-vCPU machine and takes a few minutes.
> If it gets OOM-killed, see Troubleshooting.

Expect:

```
level=INFO msg="database ready"
level=INFO msg="redis ready"
level=INFO msg="discord connected" user=Notifco guilds=1
level=INFO msg="slash commands registered" count=4
level=INFO msg="catalog loaded" term=1269 courses=7891
```

## 6. Verify

```sh
curl -s localhost:9090/healthz          # -> ok
docker compose ps                        # all three healthy
free -h                                  # confirm swap is in use, not exhausted
docker stats --no-stream                 # memory per container
```

In Discord: `/search CS 341`, then `/watch CS 341 LEC 002`.

## 7. Survive reboots

`restart: unless-stopped` plus `systemctl enable docker` covers most of it. To
manage the stack as a unit:

```sh
sudo tee /etc/systemd/system/notifco.service > /dev/null <<EOF
[Unit]
Description=notifco
Requires=docker.service
After=docker.service network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
WorkingDirectory=/home/$USER/notifco
ExecStart=/usr/bin/docker compose -f docker-compose.yml -f docker-compose.gcp.yml up -d
ExecStop=/usr/bin/docker compose -f docker-compose.yml -f docker-compose.gcp.yml down
User=$USER

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now notifco
```

## 8. Guard against surprise bills

Free tier is per-month and per-allowance; a budget alert is the cheap insurance.

**Billing → Budgets & alerts → Create budget**, scope to your project, amount **$1**,
alert at 100%. If anything ever leaves the free envelope you hear about it immediately.

Also worth checking **Billing → Reports** after a few days: usage should show as
covered by free-tier credit with a $0 subtotal.

## 9. Back up

Only Postgres holds anything irreplaceable (users, subscriptions, notification
log). Redis is a cache; the catalog refetches.

```sh
mkdir -p ~/backups
cat > ~/backup-notifco.sh <<'EOF'
#!/bin/sh
set -eu
cd "$HOME/notifco"
docker compose exec -T postgres pg_dump -U notifco notifco \
  | gzip > "$HOME/backups/notifco-$(date +%F).sql.gz"
find "$HOME/backups" -name 'notifco-*.sql.gz' -mtime +14 -delete
EOF
chmod +x ~/backup-notifco.sh
( crontab -l 2>/dev/null; echo "0 4 * * * $HOME/backup-notifco.sh" ) | crontab -
```

Restore:

```sh
gunzip -c ~/backups/notifco-YYYY-MM-DD.sql.gz \
  | docker compose exec -T postgres psql -U notifco -d notifco
```

## 10. Upgrade

```sh
cd ~/notifco && git pull
docker compose -f docker-compose.yml -f docker-compose.gcp.yml up -d --build
```

Migrations are idempotent and run on boot.

---

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `docker build` killed, or exit 137 | Out of memory compiling Go on 1 GB. Confirm swap is active (`free -h`). Failing that, build elsewhere: `docker build` in Cloud Shell, push to Artifact Registry, and pull on the VM. |
| Container restart-looping with code 137 | The OOM killer. Check `docker stats`; lower the limits in `docker-compose.gcp.yml` further. |
| Postgres `FATAL: sorry, too many clients` | `NOTIFCO_DB_MAX_CONNS` exceeds Postgres `max_connections`. The GCP override sets 6 against 30. |
| Slash commands missing | Global registration takes up to an hour. Set `DISCORD_GUILD_ID` for instant. |
| Bot online, no alerts ever | Expected. Alerts fire only on a **full → open** edge. An already-open section never notifies until it fills and frees again. |
| 401 from the UW API | Key wrong or revoked. Re-register (see the main README). |
| Unexpected charge | Almost always disk type (**Balanced** instead of **Standard**) or a non-free region. Check **Billing → Reports**, group by SKU. |

## Scaling note

The differ and notifier are safe to run concurrently — the Redis Lua CAS and
`UNIQUE(subscription_id, transition_id)` handle it. The **poller is not**: two
pollers would duplicate upstream requests and waste the rate-limit budget
without breaking correctness. Running more than one instance would need a
distributed lock on the poll schedule.

## Security

Postgres, Redis and `/metrics` are all bound to `127.0.0.1`. `/metrics` is
unauthenticated by design and must never be exposed. To view it:

```sh
gcloud compute ssh notifco --zone=us-central1-a -- -L 9090:localhost:9090
```

then open `http://localhost:9090/metrics` locally. Keep `.env` out of git — it
is already gitignored.
