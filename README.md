# Stremio AI Search Addon v2

Multi-provider, hot-reloadable, low-latency, and content-filtered natural language movie & series search addon for Stremio. Designed specifically for low-overhead, high-performance, and static compilation on bare-metal systems, Docker, and Alpine Linux LXC containers on Proxmox.

---

## What's New in v2

- **5 AI Providers:** Groq, Cerebras, Google AI Studio, Cloudflare Workers AI, and OpenRouter (with parallelized network and API-key verification).
- **Unified Request Collapsing (50% Cost Savings):** Collapses concurrent `movie` and `series` Stremio queries into **exactly one AI call** using Go's `singleflight` and in-memory caches, dropping the latency of the second request to `<1ms`.
- **Syntactic Semantic Scoring ($S_{\text{intent}}$):** A language-agnostic, deterministic NLP engine that extracts query length, grammatical structures, temporal prepositions, and capitalization patterns to classify search intent in `<1 microsecond` with zero AI cost.
- **Lexical Suffix Stripping:** Automatically identifies and strips trailing qualifiers (such as `"series"`, `"movie"`, `"tv show"`) from exact titles (e.g., `"From series"` → `"From"`), guaranteeing precise Cinemeta fuzzy matching.
- **Live Model Discovery Engine:** Dynamic backend proxy that queries official provider registries (Groq, Cerebras, Google, OpenRouter) to automatically fetch and populate the newest models directly inside the dashboard.
- **Dynamic Reasoning Suppression:** Detects reasoning models (such as Cerebras `gpt-oss-120b` or `zai-glm-4.7`) and dynamically injects parameters like `reasoning_format: "hidden"` and `thinkingConfig: { thinkingBudget: 0 }` to avoid JSON truncation errors while preserving structured output.
- **Content Filtering & Safe Search:** Toggleable dashboard options to strictly exclude Adult/NSFW or Anime/Manga content behind the scenes using dynamic system prompt injections with query-intelligence bypasses.
- **Native Alpine OpenRC Integration:** Packaged cleanly to run as an unprivileged background system service on Alpine Linux.

---

## System Architecture

```text
Stremio Client App (Concurrent Requests)
│
├─► GET /catalog/movie/ai-search/search=query.json ──┐
└─► GET /catalog/series/ai-search-series/search=.. ──┼─► [Syntactic Semantic Scoring]
                                                      │
                                                      ├─ Exact Title? ──► Cinemeta Direct (0 AI Cost)
                                                      └─ Semantic? ──► Go Singleflight Collapse
                                                                         │
                                                                         ▼
                                                            Consolidated AI Call
                                                            (suppressed reasoning, raw JSON)
                                                                         │
├─◄──────────────────────────────────────────────────────────────────────┘
│
▼
Stream Splitter
├─ Movie thread keeps "movie"
└─ Series thread keeps "series"
│
▼
Cinemeta Metadata
(Poster, Cast, Plot)
│
▼
Stremio Catalog Payload
```

---

## Production Deployment on Alpine Linux (Proxmox LXC)

Go's static compilation allows you to run this application on Alpine Linux with zero dynamic library dependencies (completely immune to `glibc` / `musl` mismatch errors).

### 1. Create an Unprivileged System User

For security, do not run the web service as `root`. Create a dedicated system user and group:

```bash
addgroup -S stremio
adduser -S -D -H -G stremio stremio
```

### 2. Extract the Release Artifacts

Create the installation folder, download the compiled `.tar.gz` archive containing the static binary and the static `/web` asset directory, and extract it:

```bash
# Create the directory
mkdir -p /opt/stremio-ai-search

# Extract your built release (tarball contains binary + web/ folder)
tar -xzf stremio-ai-search-v2.0.3-linux-amd64.tar.gz -C /opt/stremio-ai-search

# Set correct ownership
chown -R stremio:stremio /opt/stremio-ai-search
```

### 3. Ensure CA Certificates are Installed

Alpine templates are stripped and lack secure root certificates. You must install them so Go can establish secure SSL handshakes with AI provider APIs (preventing EOF or handshake failure drops):

```bash
apk update && apk add --no-cache ca-certificates
```

### 4. Create the OpenRC Init Script

Create a new service script at:

```text
/etc/init.d/stremio-ai-search
```

Edit the file:

```bash
vi /etc/init.d/stremio-ai-search
```

Copy and paste the following configuration:

```sh
#!/sbin/openrc-run

name="Stremio AI Search Addon"
description="Multi-Provider AI Movie & Series Search Addon"

# Service execution parameters
command="/opt/stremio-ai-search/stremio-ai-search"
command_args=""
command_background="yes"
directory="/opt/stremio-ai-search"

# Output and runtime files
pidfile="/run/${RC_SVCNAME}.pid"
output_log="/var/log/stremio-ai-search.log"
error_log="/var/log/stremio-ai-search.err"

# Run as the unprivileged user
command_user="stremio:stremio"

depend() {
    need net
    after firewall
}

start_pre() {
    touch "$output_log" "$error_log"
    chown stremio:stremio "$output_log" "$error_log"

    if [ ! -f "/opt/stremio-ai-search/config.json" ]; then
        echo "{}" > "/opt/stremio-ai-search/config.json"
    fi

    chown -R stremio:stremio "/opt/stremio-ai-search"
}
```

Make the script executable:

```bash
chmod +x /etc/init.d/stremio-ai-search
```

### 5. Start and Enable on System Boot

Register the service with OpenRC's default runlevel and start it:

```bash
# Enable on system boot
rc-update add stremio-ai-search default

# Start the service immediately
rc-service stremio-ai-search start
```

### 6. Monitor Logs and Operations

You can audit incoming search queries, singleflight collapsing, and active latencies by viewing the error logs (Go redirects standard telemetry to stderr):

```bash
tail -f /var/log/stremio-ai-search.err
```

---

## Configuration & First-Time Setup

### Access the Dashboard

Open your browser and navigate to the configuration dashboard:

```text
http://<your-lxc-ip>:8080/configure
```

or your reverse-proxy domain.

### Add API Keys

Configure at least one provider:

- Groq
- Cerebras
- Google AI Studio
- Cloudflare Workers AI
- OpenRouter

### Discover Models

With your API key saved, click **"Fetch Live"** next to any model input. The discovery engine will securely query the provider's active registry and populate the input field with the latest models automatically.

### Suppress Reasoning (Optional)

If configuring advanced reasoning models (like Cerebras `gpt-oss-120b` or `zai-glm-4.7`), toggle **"Disable Thinking"** to **ON**.

The server will dynamically inject the necessary parameters to:

- Drop internal reasoning text
- Reduce latency
- Prevent JSON parse truncation errors

### Install on Stremio

1. Click **Save Configuration**
2. Scroll down to the **Installation** card
3. Click **Install Addon**

This immediately triggers the `stremio://` protocol handler, launching your Stremio client app and opening the direct installation popup, bypassing manifest and browser caching issues.

---

## Directory Structure

```text
/opt/stremio-ai-search/
├── stremio-ai-search       # Executable statically compiled binary
├── config.json             # Persisted configuration file
└── web/
    └── index.html          # Lightweight, responsive config dashboard
```

---

## License

MIT

---

## Suggested Commit Message

```text
docs: update README with Alpine OpenRC service setup and v2 architecture details
```
