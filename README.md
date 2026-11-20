# omg-review-bot

A self-hosted GitHub pull request reviewer. It runs as one Go binary on Windows or Linux and uses
**LM Studio** or **Ollama** (local or Ollama Cloud) as the model. It follows a **review skill**
(`SKILL.md`) that you provide.

- Watches every repo in your org through a **GitHub App**. It polls, so you need no public URL,
  open port or webhook.
- Reviews new PRs, then **re-reviews on every push**. It waits briefly after a push and doesn't
  repeat comments it has already made.
- Posts a GitHub review: **inline comments** on changed lines plus a summary. It never approves
  or blocks a PR.
- **Replies** when someone answers one of its comments, and answers `@bot` mentions in the PR
  conversation.
- **Web UI** for settings, the skill, repositories, PR status, review history (including raw model
  output) and an activity log.

## Quick start

Requires Go 1.27+ (older Go downloads it automatically). No C compiler is needed: SQLite is pure Go.

```powershell
go install github.com/OMG-Software/omg-review-bot/cmd/prbot@latest
prbot                                  # http://127.0.0.1:8080
```

Or from a clone: `go build -o prbot.exe ./cmd/prbot` (Linux: `go build -o prbot ./cmd/prbot`).

1. Open the UI and choose an admin password.
2. **Create a GitHub App** (org → Settings → Developer settings → GitHub Apps → *New GitHub App*):
   - Homepage URL: anything. **Webhook:** untick *Active*.
   - Repository permissions: **Pull requests: Read & write**, **Contents: Read-only**, **Metadata: Read-only**.
   - Create it, note the **App ID**, then *Generate a private key* (downloads a `.pem`).
   - *Install App* on your org: either all repositories or the ones you choose.
3. In **Settings**, paste the App ID and the `.pem` contents, then click **Test GitHub connection**.
4. Choose the model provider, click **Load models**, pick one, set **Context tokens** to match the
   model, then click **Test model**. Save.
5. On the **Skill** page, upload your skill (a `.zip` of the folder or a single `SKILL.md`) or edit it in place.
6. Go to **Repositories → Sync from GitHub**. The bot reviews new pushes from then on.

### Model providers

| Provider | Base URL | Notes |
|---|---|---|
| LM Studio | `http://localhost:1234/v1` | Set the context length when you load the model in LM Studio. |
| Ollama (local) | `http://localhost:11434` | Uses the native `/api/chat` so the bot can set `num_ctx`. Ollama's default context is small and it silently cuts long prompts. |
| Ollama Cloud | `https://ollama.com` | Needs an API key from ollama.com. |
| Other OpenAI-compatible | e.g. `http://host:8000/v1` | vLLM, llama.cpp server, and others. |

If the model runs on another machine, point the base URL at it. For example, LM Studio with
"Serve on local network" enabled, or Ollama started with `OLLAMA_HOST=0.0.0.0`.

**Choosing a model:** code-focused models with at least a 32k context work best (for example
Qwen2.5-Coder 32B, Qwen3-Coder or Devstral). Reasoning models are fine because `<think>` blocks
are stripped. Leave *Enforce JSON schema output* on unless your server rejects it.

## How reviews work

1. Each poll lists open PRs per enabled repo. The bot sends an ETag with each request, so repos
   with no changes come back as `304` and don't count against the API rate limit.
2. When the head commit changes, the PR is queued after the *wait after push* delay. Reviews run
   one at a time, so a single GPU is never asked to do two at once.
3. The diff is filtered by the ignore patterns. Each line is labelled with its new-file line
   number, and the diff is split into parts that fit the context window alongside the skill
   (the skill gets at most 35% of the window).
4. The model returns JSON findings. The bot checks that each finding's line exists in the diff
   (moving it up to 3 lines to the nearest valid line if needed). It drops findings it has
   already posted, applies the minimum severity and the comment cap, and posts one review.
   Findings it can't place on a line go into the summary.
5. A failed review is retried twice with backoff. After that it waits for a new push or for
   **Review now**.

**Conversations:** when a person replies in a thread the bot started, the bot answers. It is given
the diff hunk, the current code around the line, and the whole thread. It ignores other bots,
replies at most *N* times per thread, and never answers the same message twice.

**Opting out:** add the `no-bot-review` or `skip-review` label to a PR, or use *Stop automatic reviews* on its page.

### Skill format

The skill is a Claude-style folder:

```
my-review-skill/
  SKILL.md            # YAML frontmatter (name, description) + instructions
  references/*.md     # optional; inlined into the prompt while they fit
```

Unlike an agent, a plain chat model can't open reference files when it needs them. The bot
therefore adds them to the prompt up front, smallest-first fit, within the skill's share of the
context. The Skill page shows how much of the context your skill uses.

## Running as a service

**Windows** (from an elevated PowerShell; the binary handles service start/stop itself):

```powershell
sc.exe create prbot binPath= "C:\prbot\prbot.exe -data C:\prbot\data" start= auto
sc.exe start prbot
```

When running as a service, logs go to `data\prbot.log`.

**Linux (systemd):** see [deploy/prbot.service](deploy/prbot.service).

## Flags

| Flag | Env | Default | |
|---|---|---|---|
| `-listen` | `PRBOT_LISTEN` | `127.0.0.1:8080` | Web UI address. Use `0.0.0.0:8080` to allow other machines on your LAN. |
| `-data` | `PRBOT_DATA` | `data` | Folder for the SQLite DB and skill. |
| `-reset-password` | | | Clears the admin password, then exits. |

## Security notes

- The UI listens only on localhost by default. Until a password is set, the first person to open
  it can set one.
- The GitHub key and model API key are stored in `data/prbot.db`. Keep that folder private.
- The bot's only actions are posting review comments and replies. PR content is passed to the
  model as data, and the prompts tell it to ignore instructions found there. Even if that fails,
  the worst result is an unhelpful comment.
- POST requests require a same-origin `Origin`/`Referer` header (CSRF), and the session cookie is
  HMAC-signed.

## License

Apache 2.0. See [LICENSE](LICENSE).
