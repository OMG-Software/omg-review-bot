# omg-review-bot

A self-hosted GitHub pull request reviewer. It's a single Go binary for Windows or Linux that
reviews your org's PRs with a model you run yourself (**LM Studio** or **Ollama**) or with
**Ollama Cloud**. It follows a **review skill** (`SKILL.md`) that you install. We recommend
[code-change-review](https://github.com/OMG-Software/code-change-review-skill).

- **Webhooks or polling.** It connects as a GitHub App. With a public address it acts on webhooks
  straight away. Without one it polls, so it also works on a laptop behind NAT with no open port
  or tunnel.
- **Many organizations, GitHub sign-in.** One bot can serve every org that installs its App.
  People sign in with GitHub and see only their own orgs' stats, pull requests, reviews and
  activity. Org owners manage their members and can give the org its own review settings. An
  administrator sees everything.
- **Reviews every push.** New PRs are reviewed, and each new commit triggers a re-review after
  a short wait. It doesn't repeat issues it has already raised.
- **Inline comments, graded P1–P5.** Each finding is posted on the exact changed line, labelled
  `[P1] CRITICAL`, `[P2] MAJOR`, `[P3] MINOR (Defect)`, `[P4] MINOR (Quality)` or `[P5] NIT`,
  with a summary on top. You choose the lowest level that gets posted.
- **Closes the loop.** When a later push fixes a finding, the bot replies in its thread to confirm
  the fix, then resolves the thread.
- **Optional approval.** Pick an approval level and the bot approves once nothing at that level
  or above is unresolved, and withdraws its approval if a later push brings one back.
- **Follows your project's rules.** It reads the repository's own docs (`CLAUDE.md`, `AGENTS.md`,
  `CONTRIBUTING.md`, style guides) from the base branch and reviews against them, citing the rule.
- **Conversations.** It replies when someone answers one of its comments, and answers when
  `@<app-name>` is mentioned in the PR conversation.
- **Web UI** for settings, the skill, repositories, PR status, review history (including the raw
  model output), review statistics with charts, and an activity log.

## Contents

- [Quick start](#quick-start)
- [Organizations and sign-in](#organizations-and-sign-in)
- [Webhooks](#webhooks)
- [Choosing a model](#choosing-a-model)
- [The review skill](#the-review-skill)
- [How it works](#how-it-works)
- [Settings](#settings)
- [Running as a service](#running-as-a-service)
- [Behind a reverse proxy (nginx)](#behind-a-reverse-proxy-nginx)
- [Troubleshooting](#troubleshooting)
- [Security](#security)
- [Development](#development)

## Quick start

You need Go 1.27 or newer; an older Go downloads 1.27 automatically. You don't need a C
compiler, because SQLite is pure Go.

```powershell
go install github.com/OMG-Software/omg-review-bot/cmd/prbot@latest
prbot
```

`go install` puts `prbot` in Go's bin folder (`%USERPROFILE%\go\bin` on Windows, `~/go/bin` on
Linux), so add that folder to your PATH or run the binary from there. To build from a clone
instead: `go build -o prbot.exe ./cmd/prbot` (on Linux, `-o prbot`).

Open **http://127.0.0.1:8080**. On the first visit every page leads to a setup page where you
choose the administrator password. That signs you in and takes you to **Settings** to connect
GitHub and a model, as below. To sign in with the password again later, go to
**http://127.0.0.1:8080/admin-login** (see [Administrator sign-in](#administrator-sign-in)).

### 1. Create the GitHub App

In your org go to **Settings → Developer settings → GitHub Apps → New GitHub App**. Org owners
can do this, or anyone an owner gives the *GitHub App manager* role. You don't need the GitHub
Developer Program.

| Field | Value |
|---|---|
| Homepage URL | anything, e.g. your org's page |
| Callback URL | `<bot address>/auth/github/callback`, for [GitHub sign-in](#organizations-and-sign-in). The bot shows the exact URL in **Settings → Sign-in and webhooks**. |
| Webhook → Active | **unticked** to poll, or ticked to use [webhooks](#webhooks) |
| Repository permissions | **Pull requests: Read and write** · **Contents: Read-only** · **Metadata: Read-only** |
| Organization permissions | **Members: Read-only** (lets sign-in read each person's role in the org) |
| Where can this App be installed | Only on this account, or **Any account** to serve several orgs |

After creating it, note the **App ID**. It's a number, not the `Iv1…` Client ID. Then click
**Generate a private key**, which downloads a `.pem` file.

### 2. Connect it

1. In the bot, go to **Settings → GitHub App**. Paste the App ID and the whole `.pem` file,
   including the `BEGIN`/`END` lines. Click **Test GitHub connection**, then **Save settings**.
2. On GitHub, open the App and click **Install App**. Pick your org and choose all repositories
   or the ones you want reviewed.
3. Click **Test GitHub connection** again. It should now list your org and how many repos it can see.

Once the bot has saved the key, you can delete the `.pem` file.

### 3. Connect a model

In **Settings → Model**:
1. Pick the provider.
2. Click **Load models** and choose one.
3. Set **Context tokens** to the context length the model is actually loaded with.
4. Click **Test model**, then **Save settings**.

### 4. Add your skill

Until you add one, the bot uses short built-in review guidelines. We recommend
[code-change-review](https://github.com/OMG-Software/code-change-review-skill). It isn't bundled
with the bot, so you always get its current version. Install it in either of these ways:

**Option A: link a clone (easiest to keep up to date).** Clone the skill next to the bot:

```powershell
git clone https://github.com/OMG-Software/code-change-review-skill C:\prbot\code-change-review-skill
```

In **Settings → Reviews → Skill folder**, enter the skill's folder inside the clone,
`C:\prbot\code-change-review-skill\skills\code-change-review`, and save. To update the skill
later, run `git pull` in the clone. The next review uses the new version; no restart needed.

**Option B: drop a zip on the web UI.** On the skill's GitHub page, click **Code → Download ZIP**.
Then drag the zip onto the bot's **Skill** page (anywhere on the page), or choose it with the file
picker. The zip goes to the machine the bot runs on, even if your browser is on another one, and
is installed into the bot's skill folder (`<data>/skill`), replacing the current skill. It applies
from the next review, with no restart. To update, download and drop the new zip.

How an uploaded zip is installed:

- The folder that holds `SKILL.md` becomes the skill, wherever it sits in the zip, so GitHub's
  download (`code-change-review-skill-main/skills/code-change-review/SKILL.md`) works as is.
  Files outside that folder, such as the repository README, are left out.
- A zip with several skills at the same level is refused; zip just the one you want. A zip
  without `SKILL.md`, or one over 20 MB unpacked, is refused too. In every refused case the
  current skill stays in place.
- If **Settings → Skill folder** points into a git clone (Option A), an upload never overwrites
  the clone. The zip is installed into `<data>/skill` instead, and the setting switches to it.
- The page then confirms the skill's name, where it was installed and how many reference files it
  has. The install is also recorded in the Activity log.

The **Skill** page shows the active skill's name and which reference files it sends. You can also
upload any other skill: a `.zip` of its folder, or a single `SKILL.md` to replace just that file.

### 5. Start reviewing

Go to **Repositories → Sync from GitHub**. From then on, **every new push** to an open PR is
reviewed.

> PRs that were already open the first time a repo is watched are marked `baseline` and skipped,
> so a backlog doesn't tie up your GPU for hours. To review one, open it on the **Pull requests**
> page and click **Review now**. To review the whole backlog, tick *Also review PRs that were
> already open* in Settings before syncing.

## Organizations and sign-in

Each GitHub account (an organization or a personal account) that installs the App becomes an
**organization** in the bot, and its repositories, PRs, reviews and activity belong to it. Only the
administrator password works until GitHub sign-in is set up:

1. On the App's GitHub page, copy the **Client ID** and click **Generate a new client secret**.
2. In the bot, open **Settings → Sign-in and webhooks**. Enter the client ID and secret, and set
   **Public URL** to the address people use to reach the bot (e.g. `https://prbot.example.com`).
   The section shows the callback URL to paste into the App's **Callback URL** field.
3. Save. The sign-in page now shows **Sign in with GitHub**.

**Who gets access.** Access is worked out from GitHub each time someone signs in:

| On GitHub | In the bot |
|---|---|
| Admin of an org that installed the App | **Owner** of that org |
| Member of that org | **Member**, once an owner clicks **Let every GitHub org member in** on the org's Members page; otherwise an owner adds them |
| Owner of a personal account that installed the App | **Owner** of it |

Owners can also add people by GitHub username on the org's **Members** page, change roles and
remove people. People added this way keep their access, and the role an owner gave them, whatever their GitHub
role. Access that
came from GitHub is removed at the next sign-in once it no longer applies. Removing someone in
the bot doesn't stop GitHub giving their access back at their next sign-in, so remove them on
GitHub too, or have an administrator block them. An org always keeps at least one owner.

| Role | Can |
|---|---|
| Member | See the org's overview and statistics, pull requests, reviews (including the model's raw output) and activity, and ask for a review with **Review now** |
| Owner | Also turn repositories on and off, stop reviews on a PR, manage members, and change the org's review settings |
| Administrator | Everything, in every org: the instance settings, model, skill, users and webhook deliveries |

The password administrator is always an administrator. On the **Users** page they can make a
GitHub user an administrator, or block someone, which signs them out at once.

### Administrator sign-in

The administrator password has its own sign-in page, **`/admin-login`** (for example
`https://prbot.example.com/admin-login`). The normal sign-in page, `/login`, only offers
**Sign in with GitHub** and doesn't link to it, so bookmark it.

- **First run.** You set the password on the setup page and are signed in at once, so you don't
  need `/admin-login` until you next sign in.
- **Before GitHub sign-in is set up**, the password is the only way in, so `/login` sends you to
  `/admin-login` automatically.
- **Signing out** as the administrator returns you to `/admin-login`; GitHub users return to
  `/login`.
- **Forgot the password?** Stop the bot and run `prbot -reset-password`, then set a new one on the
  setup page. Setting it signs everyone out, so sessions from before the reset stop working.
  Until a password is set, **anyone who can reach the bot can set it**, so do this before the bot
  is reachable from the internet again (keep it stopped, or block `/setup` at the proxy), and set it
  straight away.

Keeping the page separate keeps the password form away from everyday users. It isn't a secret,
though; the password is what protects it. To limit who can reach it, restrict the path in your
reverse proxy (see [Behind a reverse proxy](#behind-a-reverse-proxy-nginx)). GitHub users who are
administrators sign in with GitHub as usual.

**Statistics.** Each org's **Overview** shows, for the last 7, 30 or 90 days: reviews posted,
findings by level, confirmed fixes, approvals, average review time and model tokens, plus a chart
of reviews per day and a breakdown by repository. Hover a bar for its numbers, or open **Show as a
table**. The administrator's dashboard shows the same totals for every org.

**Per-org review settings.** An org uses the instance's review settings until an owner saves its
**Review settings** page. From then on it has its own: wait after push, comment and approval
levels, comment cap, drafts, skip labels, guideline and ignore files, and replies. **Use instance
defaults instead** removes them. The model, the skill and the review queue stay shared, so every
org's reviews take turns on the same GPU.

## Webhooks

Polling works anywhere but reacts within a poll interval. With webhooks, GitHub tells the bot about
pushes, new PRs, replies and installations as they happen. The bot must be reachable from GitHub
at its **Public URL**.

1. In the bot's **Settings → Sign-in and webhooks**, click **Generate** next to **Webhook secret**,
   copy it and save.
2. In the App's GitHub settings, tick **Webhook → Active**. Set **Webhook URL** to
   `<public URL>/webhooks/github` and **Webhook secret** to the same value.
3. Under **Subscribe to events**, tick **Pull request**, **Pull request review**, **Pull request
   review comment**, **Pull request review thread**, **Issue comment** and **Repository**.
   Installation events are always sent.

The **Webhooks** page (administrators) lists recent deliveries and what the bot did with each.
Every delivery's `X-Hub-Signature-256` is checked against the secret; unsigned or wrongly signed
ones are refused, and a redelivery of the same event is ignored. With no secret set, the endpoint
returns `404`.

Polling carries on as a safety net. While deliveries are arriving (one in the last 24 hours), the
bot polls only every *While webhooks arrive, poll every* seconds (15 minutes by default) instead of
every *Poll interval*.

## Choosing a model

| Provider | Base URL | Notes |
|---|---|---|
| LM Studio | `http://localhost:1234/v1` | Context length is set when you load the model in LM Studio; the bot can't change it. |
| Ollama (local) | `http://localhost:11434` | Uses Ollama's native `/api/chat` so the bot can set `num_ctx`. Ollama's OpenAI-compatible endpoint can't, and its small default silently drops the start of long prompts. |
| Ollama Cloud | `https://ollama.com` | Needs an API key from ollama.com. |
| Other OpenAI-compatible | e.g. `http://host:8000/v1` | vLLM, llama.cpp server, and others. |

To use a model on another machine, set the base URL to that machine. In LM Studio, enable
*Serve on local network*. For Ollama, start it with `OLLAMA_HOST=0.0.0.0`.

**What works well:** code models of 14B parameters or more with at least a 32k context, for
example Qwen2.5-Coder 32B, Qwen3-Coder 30B-A3B or Devstral. Small general models (around 4B)
produce valid output but tend to summarize the PR and find nothing.

**Reasoning models** (Qwen3, DeepSeek-R1, …) work. Their `<think>` blocks are removed, and
reasoning returned in a separate field is ignored. However, they spend output tokens on thinking
before answering. If a review fails with *"used all its output tokens on reasoning"*, raise
**Max output tokens** (8192 or more) or use a non-reasoning model.

Leave **Enforce JSON schema output** on unless your server rejects `response_format` (OpenAI
style) or `format` (Ollama). With it on, the model's reply is constrained to valid review JSON.

## The review skill

A skill is a Claude-style folder:

```
my-review-skill/
  SKILL.md                    # YAML frontmatter (name, description) + review instructions
  references/
    review-angles.md          # general references: always sent
    reporting.md
    languages/
      c-cpp.md                # language references: sent only when the PR touches that language
      shell.md
      infrastructure.md
      ...
```

Claude opens reference files only when it needs them. A plain chat model can't, so the bot
chooses the files before each review:

- **General references** (`.md`, `.markdown`, `.txt` anywhere outside a `languages/` folder)
  are always included.
- **Language references** (inside a folder named `languages/`, `language/`, `langs/` or
  `lang/`) are included only when the PR changes files in that language. The file name is split
  on `-` and each part is compared with the PR's file types. For example:

  | Reference | Included when the PR changes |
  |---|---|
  | `c-cpp.md` | `.c` `.h` `.cpp` `.hpp` `.cc` …, `CMakeLists.txt` |
  | `csharp-dotnet.md` | `.cs` `.csproj` `.razor` … (not `.c`) |
  | `java-kotlin.md` | `.java` `.kt` `.gradle` (not `.js`) |
  | `javascript-typescript.md` | `.js` `.ts` `.tsx` `.vue` … |
  | `shell.md` | `.sh` `.bash` `.ps1` … |
  | `infrastructure.md` | `.yml`/`.yaml`, `.github/workflows/`, Dockerfiles, Terraform, `k8s/`, `helm/` … |

  The other recognized names are `go`, `python`, `ruby`, `rust`, `php`, `sql`, `swift`, `kotlin`,
  `typescript`, `terraform`, `docker`, `yaml`, `ci` and `powershell`, among others.

The skill may use at most **35%** of the model's context; references that don't fit are left
out. The **Skill** page shows which references are *always* or *per language* sent and how many
tokens each takes. Each review's raw output begins with a `skill references sent:` line.

Reviews follow the bot's own output format (JSON with path, line, severity, title and body). If
your skill describes a different report format, that part is overridden. Its review criteria
still apply. Skills that grade with `[CRITICAL]`/`[MAJOR]`/`[MINOR]`/`[NIT]` (as code-change-review
does) are mapped onto the bot's levels: CRITICAL → P1, MAJOR → P2, a MINOR defect → P3, a MINOR
quality issue → P4, NIT → P5. Each comment's body has an *Issue / Why it matters / Suggested fix*
shape.

| Level | Meaning |
|---|---|
| `[P1] CRITICAL` | Security hole, data loss or corruption, crash on a main path |
| `[P2] MAJOR` | Wrong behaviour people will hit, or a serious risk under realistic conditions |
| `[P3] MINOR (Defect)` | A real defect with limited impact, such as an edge case or a missing timeout |
| `[P4] MINOR (Quality)` | Not a bug: maintainability, duplication, missing tests, clarity |
| `[P5] NIT` | Style or taste with no behavioural effect |

### Project guidelines

Besides the skill, the bot reads the repository's own rules and conventions and gives them to the
model with every review, thread reply and mention. The model applies them, cites the file and
rule in findings that rest on them, and doesn't flag what they explicitly allow. Real defects are
reported regardless. This works with any skill: a chat model can't open files itself, so the
bot fetches them and puts them in the prompt.

- **Which files:** set by *Project guideline files* in Settings, as gitignore-style patterns. By
  default: `CLAUDE.md` and `AGENTS.md` in any directory, plus root-level `CONTRIBUTING.md` (also in
  `.github/` and `docs/`), `.github/copilot-instructions.md`, `STYLEGUIDE.md`, `STYLE_GUIDE.md`,
  `CODING_STANDARDS.md` and `CONVENTIONS.md`. Files matching *Ignore files* (such as `vendor/`)
  are skipped. Clear the list to turn this off.
- **Scope:** a `CLAUDE.md` or `AGENTS.md` in a subdirectory is only sent when the PR changes files
  under that directory, and takes precedence over the root one there. Other files apply to every PR.
- **Base branch:** the files are read from the PR's base commit, not its head, so a PR can't
  rewrite the rules it's reviewed against. Changes a PR makes to them are reviewed like any other
  change.
- **Budget:** they get up to 15% of the model context in reviews (10% in replies), root files first.
  Files that don't fit are left out and logged. Each review's raw output lists them under
  `project guidelines sent:`.
- **API use:** one repository file listing plus one request per matching file, cached for 10
  minutes per repository and base.

## How it works

**Polling.** Every poll, the bot lists open PRs in each enabled repo, sending the ETag from the
previous response. Repos with no changes return `304 Not Modified`, which doesn't count against
the API rate limit. The repo list itself refreshes every 15 minutes by default. With
[webhooks](#webhooks), a pull request event updates the PR at once, and a reply or review-thread
event checks it for replies, so polling only catches anything missed.

**Reviewing.**

1. When a PR's head commit changes, the PR becomes `pending`. After the *wait after push*
   delay, it's queued. Reviews run one at a time, so a single GPU never gets two requests at once.
2. Files that are ignored, deleted or binary are skipped. Each remaining diff line is labelled
   with its new-file line number, so the model doesn't have to work out line numbers inside diff
   hunks. Large diffs are split into parts that fit the context, and the parts' summaries are
   combined at the end. If there's anything to review, the bot first posts a PR comment saying the
   review has started, with the commit, the start time (UTC), the model and the review skill.
   A PR with nothing reviewable gets no such comment. When the review is posted, the comment is
   edited to say so, with a link to it. If the review fails, the comment is edited to say that
   instead (without the error, which is on the PR's page in the bot), and a retry of the same
   commit reuses it.
3. The model returns JSON findings. The bot checks each finding's file and line against the
   diff, moving it to the nearest changed line up to 3 lines away if needed. It drops findings
   the bot already raised in an earlier review. It drops findings below the *comment level* and
   applies the comment cap.
4. On a re-review, it first checks its unresolved findings on files that changed since the last
   review. For each file it gives the model the current file and the earlier findings, and asks
   which are fixed. For each confirmed fix it **replies in the thread** ("✅ Confirmed fixed in
   `abc1234`…") **and then resolves the thread**. A finding whose file the PR no longer changes is
   closed the same way. Anything the model isn't sure about stays open.
5. It posts **one** GitHub review: inline comments plus a summary. Findings that can't be placed
   on a changed line go in the summary. If GitHub rejects an inline position, the review is
   reposted with every finding in the summary, so nothing is lost.
6. With an *approval level* set, the review is an **approval** when no finding at that level or
   above is open: no unresolved earlier thread and no new finding. Otherwise it's a comment that
   says how many are open, and any earlier approval by the bot is dismissed (a later comment
   doesn't cancel an approval on GitHub). Approval is re-checked between pushes too, so resolving
   the last open thread, by fix or by hand, gets the PR approved. A reply in the PR triggers the
   check within a poll; a thread resolved by hand with no new comment isn't visible in the PR list,
   so it's picked up by the 15-minute re-check instead.
7. A failed review is retried twice, after 2 and 8 minutes. After that the PR shows `error`
   until the next push or **Review now**.

**PR statuses** on the Pull requests page:

| Status | Meaning |
|---|---|
| `pending` | New commits waiting to be reviewed (or waiting out the *wait after push* delay) |
| `reviewing` | A review is running |
| `reviewed` | The current head commit has been reviewed |
| `baseline` | Was already open when the repo was first watched; will be reviewed on the next push |
| `draft` | Draft PR; reviewed once it's marked ready (if *Skip drafts* is on) |
| `skipped` | Has a skip label, or automatic reviews were stopped on its page |
| `error` | Review failed 3 times; see the PR page for the error |
| `closed` | Closed or merged |

**Conversations.** When someone replies in a thread the bot started, the bot answers in that
thread. It's given the diff hunk, the current code around the line (±30 lines) and the whole
thread (for an outdated comment, ±60 lines). It can agree that an issue is resolved, defend its
comment, or answer the question. When it agrees the concern no longer applies, because the code
now fixes it or the person showed it isn't a real problem, it says so in its reply and then
resolves the thread.
When someone writes `@<app-name>` in the PR conversation, the bot answers there, using the
changed-file list, its latest review summary and the recent comments.

The bot ignores other bots, replies at most *Max bot replies per thread* times per thread, and
never answers the same message twice. As a backup, PRs with activity in the last 14 days are
re-checked for replies every 15 minutes.

**Opting out:** add a `no-bot-review` or `skip-review` label to a PR, or click *Stop automatic
reviews* on its page in the UI.

## Settings

Everything except the command-line flags is set in the web UI and stored in the database. The
review settings below (from *Wait after push* to *Replies / mentions*) are the instance
defaults; an org owner can override them for their org (see
[Per-org review settings](#organizations-and-sign-in)).

| Setting | Default | |
|---|---|---|
| Wait after push | 60 s | Delay before reviewing, so several quick pushes trigger one review |
| Max comments per review | 20 | The most severe findings are kept |
| Post comments at | `[P5] NIT` and above | Lower levels are counted in the summary, not posted. Replaces *Minimum severity*: a saved `nit`/`minor`/`major`/`critical` becomes P5/P4/P2/P1. |
| Approve when nothing is unresolved at | Never approve | A level from P1 to P5. Can't be broader than the comment level. For example, comments at P4 with approval at P3: the bot approves when no P1–P3 finding is open, even with open P4 comments. |
| Max files per PR | 150 | Remaining files are listed as not reviewed |
| Skip drafts | on | |
| Skip labels | `no-bot-review`, `skip-review` | |
| Project guideline files | `CLAUDE.md`, `AGENTS.md`, root `CONTRIBUTING.md`, style guides … | See [Project guidelines](#project-guidelines). Empty = off. |
| Ignore files | lockfiles, `go.sum`, `vendor/`, `node_modules/`, `dist/`, `*.min.js`, `*.map`, `*.pb.go`, `*.snap`, `*.svg` | gitignore-style patterns |
| Replies / mentions | on / on | Max 5 bot replies per thread |
| Poll interval | 60 s | Repo list refreshes every 15 min |
| While webhooks arrive, poll every | 900 s | Used instead of *Poll interval* while webhook deliveries arrive (one in the last 24 h) |
| Public URL / Client ID / Client secret / Webhook secret | empty | See [Organizations and sign-in](#organizations-and-sign-in) and [Webhooks](#webhooks) |
| Context tokens / Max output tokens | 32768 / 4096 | Must match how the model is loaded |
| Temperature | 0.2 | |

### Command-line flags

| Flag | Environment variable | Default | |
|---|---|---|---|
| `-listen` | `PRBOT_LISTEN` | `127.0.0.1:8080` | Web UI address. Use `0.0.0.0:8080` to allow other machines on your LAN. |
| `-data` | `PRBOT_DATA` | `data` | Folder for the database and skill. A relative path is resolved next to the binary when running as a Windows service. |
| `-reset-password` | | | Clears the admin password so you can set a new one, then exits. |

## Running as a service

**Windows.** The binary detects when the Windows Service Control Manager starts it, so no
wrapper is needed. Run this in an elevated PowerShell:

```powershell
sc.exe create prbot binPath= "C:\prbot\prbot.exe -data C:\prbot\data" start= auto
```
```powershell
sc.exe start prbot
```

As a service, it writes logs to `data\prbot.log`. Stop any copy running in a console first,
because both would use port 8080. To move an existing setup, copy its `data` folder.

**Linux (systemd).** See [deploy/prbot.service](deploy/prbot.service) for the unit file and
install commands. It writes logs to the journal: `journalctl -u prbot -f`.

**Updating.** Run `go install github.com/OMG-Software/omg-review-bot/cmd/prbot@latest` (or
rebuild), then restart the bot. Reviews that were interrupted are re-queued on startup.

## Behind a reverse proxy (nginx)

GitHub sign-in and webhooks need the bot to be reachable over HTTPS. The usual way is to keep the
bot on its default `127.0.0.1:8080` and put nginx in front of it to handle TLS.

**1. Get a certificate first.** nginx refuses a configuration whose certificate files don't
exist yet, so start with only the port-80 block below (the first `server { … }`), reload nginx,
and get the certificate:

```bash
sudo certbot certonly --nginx -d prbot.example.com
```

**2. Add the HTTPS block** and reload nginx (`sudo nginx -t && sudo systemctl reload nginx`):

```nginx
server {
    listen 80;
    server_name prbot.example.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    server_name prbot.example.com;

    ssl_certificate     /etc/letsencrypt/live/prbot.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/prbot.example.com/privkey.pem;

    # Keep browsers on HTTPS (see the note on cookies below).
    add_header Strict-Transport-Security "max-age=31536000" always;

    # Skill zip uploads can be up to 25 MB, and webhook payloads up to 5 MB (nginx allows 1 MB by default).
    client_max_body_size 25m;

    location / {
        proxy_pass http://127.0.0.1:8080;

        # Required: the bot blocks a form post unless the browser's Origin matches this host.
        proxy_set_header Host $host;

        # "Test model" waits for the model's reply, up to the LLM timeout (600 s by default).
        proxy_read_timeout 600s;
    }
}
```

Then, in the bot:

1. Open **Settings → Sign-in and webhooks** and set **Public URL** to `https://prbot.example.com`.
   Behind a proxy the bot only sees a plain HTTP connection from nginx, so this is how it knows the
   address to use for the sign-in callback and the webhook URL.
2. Copy the callback and webhook URLs the section shows into the GitHub App, as in
   [Organizations and sign-in](#organizations-and-sign-in) and [Webhooks](#webhooks).

Notes:

- **Keep the `Host` header.** Without `proxy_set_header Host $host`, nginx sends `127.0.0.1:8080`,
  and every save, sign-out and button press fails with *cross-origin request blocked*. If nginx
  listens on a port other than 443, use `$http_host` instead, which includes the port.
- **Cookies.** The bot marks its cookies `Secure` when a request arrives on an `https://`
  **Public URL** (which is why nginx must pass the `Host` header) or when it terminates TLS
  itself. A direct plain-HTTP visit, such as `http://127.0.0.1:8080` on the server, gets ordinary
  cookies so it still works. The `Strict-Transport-Security` header keeps browsers on HTTPS.
- **Don't expose port 8080.** Keep `-listen 127.0.0.1:8080` so the only way in is through nginx.
- **Restricting the administrator sign-in (optional).** To allow the password sign-in only from
  your own network, add this inside the `server` block. Everyone else gets `403` there and can
  still sign in with GitHub:

  ```nginx
  location = /admin-login {
      allow 10.0.0.0/8;     # your office or VPN range
      deny  all;
      proxy_pass http://127.0.0.1:8080;
      proxy_set_header Host $host;
  }
  ```
- **Renewing the certificate.** certbot's timer renews it; `sudo certbot renew --dry-run` checks
  that renewal works.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| GitHub test fails with `401` or `404` | Wrong App ID (use the number, not the `Iv1…` Client ID), the wrong App's key, or the PEM is missing its `BEGIN`/`END` lines. Also check the PC's clock: the App's sign-in token is time-limited. |
| GitHub test: "isn't installed anywhere yet" | The credentials work; install the App on your org (step 2). |
| No repositories listed | The App isn't installed on them, or the *Only these orgs* setting excludes the org. Click **Sync from GitHub**. |
| An open PR is never reviewed | It's probably `baseline` (it was open before the repo was watched). Push to it or click **Review now**. |
| Reviews post with zero findings | Open the review's **Raw model output**. An empty `"comments": []` means the model found nothing, which small models often do, so try a larger code model. If the raw output has comments that weren't posted, they were filtered as duplicates or below the comment level. |
| Fixed findings aren't resolved, or the bot never approves | Thread states come from GitHub's GraphQL API. Check the activity log for "could not read review thread states". While they can't be read, every finding counts as open, so the bot doesn't approve. The model's fix checks are in the review's raw output under `fix check:`. |
| "could not withdraw approval" | The base branch's protection limits who can dismiss reviews. Allow the App to dismiss reviews, or dismiss the approval by hand. |
| "used all its output tokens on reasoning" | Reasoning model; raise **Max output tokens** or switch model. |
| "model context … is too small" | Raise **Context tokens** (and the model's loaded context), or shorten the skill. |
| "did not return valid JSON" | Turn on **Enforce JSON schema output**. If your server rejects it, use a stronger model. |
| Forgot the admin password | Stop the bot and run `prbot -reset-password`. |
| The sign-in page has no password field | It's on its own page: go to `/admin-login`. See [Administrator sign-in](#administrator-sign-in). |
| GitHub sign-in: "redirect_uri is not associated with this application" | The App's **Callback URL** doesn't match. Copy it from **Settings → Sign-in and webhooks**, and set **Public URL** if the bot is behind a proxy. |
| Signed in but no organizations listed | The App isn't installed on your org, or you're a plain member and the org hasn't let members in: ask an owner to add you. |
| "Couldn't read your role in …" | Give the App the **Members: Read-only** organization permission, then accept the new permission on the org's installation page. Until GitHub can confirm someone's role, access that came from GitHub is paused; access added by hand isn't affected. |
| Behind a proxy, every save or button fails with "cross-origin request blocked" | The proxy isn't passing the original `Host` header. See [Behind a reverse proxy](#behind-a-reverse-proxy-nginx). |
| Webhook deliveries fail with `413` | The proxy's request size limit is too small; set `client_max_body_size 25m` in nginx. |
| Webhook deliveries fail with `401` | The secret in GitHub differs from the bot's. Set them to the same value. |

## Security

- The UI listens on `127.0.0.1` only by default. Until an admin password is set, anyone who can
  open the UI can set one, so set it right away if you change `-listen`.
- The GitHub App private key and model API key are stored unencrypted in `data/prbot.db`. Keep
  the `data` folder private; it's gitignored.
- The bot's only GitHub actions are posting reviews, review replies and PR comments. The App
  permissions above are the minimum it needs.
- PR titles, descriptions and code are untrusted. They're given to the model as data, and the
  prompts tell it to ignore instructions inside them. If an injected instruction got through
  anyway, the worst outcome is a misleading comment: the model has no tools and can't take
  actions.
- The UI uses an HMAC-signed session cookie and PBKDF2-hashed password. It rejects
  cross-origin POSTs (CSRF) and sends a strict Content-Security-Policy.
- GitHub sign-in uses a one-time `state` check. The bot keeps each person's GitHub ID, login,
  name and avatar, never their GitHub token. A person sees only the orgs they belong to; asking
  for another org's page, PR or review returns `404`.
- **Sign out everywhere** (Account page) ends all of a GitHub user's sessions, and blocking a user
  ends theirs at once. Changing the administrator password signs everyone out.
- The administrator password is entered only at `/admin-login`, which the normal sign-in page
  doesn't link to. Wrong passwords are slowed down; you can also restrict the path in your
  reverse proxy.
- The client secret and webhook secret are stored in `data/prbot.db` like the other keys.
  Webhook deliveries must carry a valid `X-Hub-Signature-256`.

## Development

```powershell
go test ./...
```
```powershell
go vet ./...
```
```powershell
go run ./cmd/prbot -data data-dev -listen 127.0.0.1:8087
```

`data-dev/` is gitignored, so a development instance can't touch your real database.

| Package | Responsibility |
|---|---|
| `cmd/prbot` | Entry point, flags, Windows service / signal handling |
| `internal/bot` | Polling, webhook events (`webhook.go`), the job queue and worker, reviews, replies, prompts, fix checks and approval (`resolve.go`) |
| `internal/github` | GitHub App auth (JWT, installation tokens), the REST calls used, GraphQL for review threads, and sign-in and webhook signatures (`oauth.go`) |
| `internal/llm` | OpenAI-compatible and native Ollama clients, JSON extraction |
| `internal/skill` | `SKILL.md` parsing, zip install, language-aware reference selection |
| `internal/diff` | Unified-diff parsing and line-numbered rendering |
| `internal/store` | SQLite schema, migrations and queries; users, orgs, memberships, deliveries and statistics (`tenancy.go`) |
| `internal/settings` | Settings model, defaults and per-org review policy |
| `internal/web` | Web UI: handlers, sessions (`auth.go`), GitHub sign-in (`oauth.go`), org pages (`orgs.go`), admin pages and the webhook endpoint (`admin.go`), SVG charts (`charts.go`), embedded templates and static files |

`internal/bot/bot_test.go` runs the bot end to end against fake GitHub and fake model servers.
It's the best place to start when changing review or reply behaviour.
`internal/web/multiuser_test.go` covers GitHub sign-in, org access and webhook deliveries against
a fake GitHub.

## License

Apache 2.0. See [LICENSE](LICENSE).
