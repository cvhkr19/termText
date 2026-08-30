# Deploying your own server

This walks through getting the server running on a real VM with a real
domain and TLS — everything the root README's quickstart skips over.
Provider-agnostic: it works on Oracle Cloud's Always Free tier, a $5
DigitalOcean droplet, AWS, or anything else that hands you a Linux box with
a public IP and SSH access.

## 1. Get a VM

Any small Ubuntu (or Debian) box with a public IPv4 address works. This
app is lightweight — a single Go binary plus SQLite — so even the smallest
tier of any provider is enough. If you want a genuinely free option,
Oracle Cloud's Always Free tier includes an ARM instance with real
specs (currently 2 OCPU / 12GB RAM), though its signup process can be
finicky (card verification, occasional "out of capacity" errors) and its
free-tier limits have changed before without much notice — don't build
around it being permanent.

Once it's up, note its public IP and confirm you can SSH in:

```
ssh ubuntu@<vm-public-ip>
```

## 2. Install Docker

```
curl -fsSL https://get.docker.com | sh
sudo usermod -aG docker $USER
```

Log out and back in for the group change to apply (or just prefix
commands with `sudo` for now).

## 3. Get the code and start the server

```
git clone https://github.com/cvhkr19/termText
cd termText
docker compose up -d --build
```

This builds the image locally on the VM and starts it — no image
registry needed. The `docker-compose.yml` in this repo already mounts a
named volume at `/data`, so the SQLite database and any uploaded files
survive a `docker compose down` or a rebuild.

Before starting it for real, open `docker-compose.yml` and set:

- `ALLOWED_ORIGINS` — leave empty unless you're building a browser
  client against this server. The TUI client sends no `Origin` header
  and is unaffected either way.
- `REGISTRATION_CODE` — uncomment and set this if you don't want the
  server open to anyone who finds the address.

Confirm it's actually listening:

```
curl localhost:8080/login -X POST -d '{}'
```

Any JSON error response (not a connection failure) means the server is up.

## 4. Point a domain at it

Add an `A` record for your domain pointing at the VM's public IP. If
you're using Cloudflare for DNS and want its proxying, keep the record
**grey-clouded ("DNS only")**, not orange — Caddy (next step) needs to
complete its own TLS handshake directly with the VM, and Cloudflare's
proxy sitting in front of that breaks it unless you set up a DNS-01
challenge instead, which this guide doesn't cover.

## 5. Install Caddy for TLS

Caddy gets you a real, auto-renewing HTTPS certificate with almost no
config — it handles the entire Let's Encrypt handshake itself.

```
sudo apt install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt update
sudo apt install caddy
```

Edit `/etc/caddy/Caddyfile` — replace its contents with:

```
chat.yourdomain.com {
    reverse_proxy localhost:8080
}
```

Then:

```
sudo systemctl reload caddy
```

Caddy will fetch a certificate for that domain automatically the first
time it receives a request for it. Your server is now reachable at
`https://chat.yourdomain.com` / `wss://chat.yourdomain.com`.

## 6. Open the firewall

Make sure ports 80 and 443 are reachable from the internet. Two things
commonly block this:

- The VM's own OS firewall (`ufw`, if enabled): `sudo ufw allow 80,443/tcp`
- A separate cloud-level firewall your provider manages outside the VM
  (Oracle calls this a "security list," AWS a "security group," etc.) —
  this one's easy to forget since it's not visible from inside the VM at
  all. Check your provider's console.

Port 8080 does **not** need to be open publicly — only Caddy (on 80/443)
talks to it, over localhost.

## 7. Test end to end

From your own machine, with the real client:

```
./termtext-client -server chat.yourdomain.com -tls
```

If it reaches the login screen and you can register, you're live.

## Hosting from your own machine instead of a VM

You don't strictly need a VM — a spare computer, a Raspberry Pi, or even
your regular laptop running `docker compose up -d` works the same way
functionally. Two things make this genuinely different from a VM in
practice, though, and worth knowing before you commit to it:

- **You almost certainly don't have a real public IP.** Most home
  internet connections sit behind CGNAT (your ISP's own NAT layer), which
  means port-forwarding on your router often does nothing at all — the
  traffic never reaches your router in the first place, no matter how
  it's configured.
- **Even when you do have one, it's usually dynamic** — it changes
  periodically, which breaks a plain DNS `A` record pointing at it.

The practical way around both problems is the same tool used earlier in
this project's own development: a tunnel (e.g. `cloudflared`) instead of
port-forwarding. It makes an outbound-only connection from your machine
to Cloudflare's network, so it works from behind any NAT/CGNAT with no
public IP and no router configuration at all, and the public hostname
stays stable even if your home IP changes underneath it. See
Cloudflare's own Tunnel docs for the setup — the short version is
`cloudflared tunnel create`, `cloudflared tunnel route dns` to point a
subdomain at it, then `cloudflared tunnel run` alongside
`docker compose up -d`.

The real tradeoff versus a VM isn't technical capability — it's exposure
and uptime. The server is only reachable while that machine and the
tunnel process are both actually running (closing the laptop lid ends
the demo), and it's sitting on the same home network as your other
devices rather than in an isolated cloud environment. Fine for testing,
a live demo you're personally driving, or genuinely casual self-hosting
among friends — not something to point a domain at and forget about the
way you could with a VM.

## Keeping it running

`docker compose up -d` already restarts the container automatically if
it crashes (see `restart: unless-stopped` in `docker-compose.yml`).
Caddy's certificate renews itself with no action needed. To update to a
newer version of the app later:

```
git pull
docker compose up -d --build
```

Your data survives this — it lives in the named volume, not the
container.
