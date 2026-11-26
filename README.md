<p align="center">
  <img src="internal/web/static/icon.svg" alt="OMG Review Bot" width="128" height="128">
</p>

# omg-review-bot

A self-hosted GitHub pull request reviewer. It's a single Go binary for Windows or Linux that
reviews your org's PRs with a model you run yourself (**LM Studio** or **Ollama**), a hosted one
(**Ollama Cloud**, **OpenRouter**, the **Anthropic API** or the **OpenAI API**), or the models of
your **ChatGPT plan** (Sign in with ChatGPT). It follows a **review skill** (`SKILL.md`) that you
install. We recommend
[code-change-review](https://github.com/OMG-Software/code-change-review-skill).

- **Webhooks or polling.** It connects as a GitHub App. With a public address it acts on webhooks
  straight away. Without one it polls, so it also works on a laptop behind NAT with no open port
  or tunnel.
- **Many organizations, GitHub sign-in.** One bot can serve every org that installs its App.
  People sign in with GitHub and see only their own orgs' stats, pull requests, reviews and
  activity. Org owners manage their members and can give the org its own review settings. An
  administrator sees everything.
- **Reviews every push.** New PRs are reviewed, and each new commit triggers a re-review after
  a short wait. A re-review raises new findings only on the code changed since the last review,
  and doesn't repeat issues it has already raised.
- **Inline comments, graded P1–P5.** Each finding is posted on the exact changed line, labelled
  `[P1] CRITICAL`, `[P2] MAJOR`, `[P3] MINOR (Defect)`, `[P4] MINOR (Quality)` or `[P5] NIT`,
  with a summary on top. You choose the lowest level that gets posted.
- **Closes the loop.** When a later push fixes a finding, the bot replies in its thread to confirm
  the fix, then resolves the thread.
- **Approves and requests changes.** The bot requests changes while a finding at the
  request-changes level (P3 by default) or above is unresolved. It approves once nothing at the
  approval level (P3 by default) or above is. It withdraws either one when it no longer holds.
  Each org can set its own levels, or turn either off.
- **Knows when to stop.** Each review says its number on the PR ("Bot review number 3 of 10").
  After the last one (10 by default, 5 to 30 per org), new pushes aren't reviewed and the PR gets
  a comment saying the limit is reached. **Review now** still works.
- **Caps each org's daily reviews.** An org gets at most 100 reviews in any 24 hours by default.
  Administrators can change that per org; org owners can see the limit but not change it.
- **Several models, or the org's own.** Configure several models and use one, fail over between
  them, take turns, or have every model review each PR and merge their findings. An org can bring
  its own models and API keys: it then uses only those, and the daily limit no longer applies.
- **Shares the review slots fairly.** One org's jobs hold at most half of the review slots at once
  by default (administrators can change the share), so a busy org can't hold up the others.
- **Follows your project's rules.** It reads the repository's own docs (`CLAUDE.md`, `AGENTS.md`,
  `CONTRIBUTING.md`, style guides) from the base branch and reviews against them, citing the rule.
- **Conversations.** It replies when someone answers one of its comments, and answers when
  `@<app-name>` is mentioned in the PR conversation. Anyone with write access can comment
  `@<app-name> review` to ask for a review, like **Review now** in the web UI.
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
- [Linux packages](#linux-packages)
- [Behind a reverse proxy (nginx, Apache)](#behind-a-reverse-proxy)
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
instead: `go build -o prbot.exe ./cmd/prbot` (on Linux, `-o prbot`). On Windows, `run.bat` in the
clone does both: it builds `prbot.exe` and starts it, passing any arguments on (for example
`run.bat -listen 0.0.0.0:8080`), and uses the clone's own `data` folder. On Linux you can
install a package instead, which runs the bot as a systemd service: see
[Linux packages](#linux-packages). `prbot -version` prints the version and the commit it was
built from.

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
| Where can this App be installed | Only on this account, or **Any account** to serve several orgs (see [Adding more organizations](#adding-more-organizations)) |

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

In **Settings → Models → Manage models**:
1. Pick the provider.
2. Click **Load models** and choose one.
3. Leave **Context tokens** and **Max output tokens** empty to use the model's maximums: the bot
   reads them from the model's server when you save (or click **Use model maximums** to see them
   first). LM Studio, Ollama, OpenRouter, the Anthropic API, vLLM and llama.cpp's server report
   them. A server that reports a context window but no output cap gets half the window for output,
   leaving the other half for the prompt. A server that reports nothing gets 131072 / 16384, and the
   page says so. For LM Studio, load the model with that context length; for Ollama the context sets
   `num_ctx`, which also sets how much memory the model takes, so lower it if your machine can't
   hold it.
4. Click **Test model**, then **Save**.

You can add more models (up to 8) and choose how they're used:

| Strategy | What happens |
|---|---|
| One model | Every review and reply uses the selected model; the others are kept, ready to switch to. |
| Failover | The first model in the list; if it fails or times out, the next one takes over. |
| Round-robin | Each review or reply uses the next model in turn. |
| Collate | Every model reviews each PR; their findings are merged, duplicates removed, keeping the more severe level. |

With several models, prompts are sized to fit the one with the smallest context.

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
> page and click **Review now**, or comment `@<app-name> review` on the PR. To review the whole
> backlog, tick *Also review PRs that were already open* in Settings before syncing.

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
remove people. People added this way keep their access, and the role an owner gave them,
whatever their GitHub role. Access that came from GitHub is removed at the next sign-in once it
no longer applies. Removing someone in the bot doesn't stop GitHub giving their access back at
their next sign-in, so remove them on GitHub too, or have an administrator block them.

Owners can't remove or demote the last owner of an org. For access that came from GitHub,
though, GitHub decides: if an org's only owner stops being a GitHub admin of it, they lose the
owner role at their next sign-in, even if that leaves the org with no owner. It gets one back as
soon as one of its current GitHub admins signs in, or an administrator makes someone an owner.

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
reverse proxy (see [Behind a reverse proxy](#behind-a-reverse-proxy)). GitHub users who are
administrators sign in with GitHub as usual.

**Statistics.** Each org's **Overview** shows, for the last 7, 30 or 90 days: reviews posted,
findings by level, confirmed fixes, approvals, average review time and model tokens, plus a chart
of reviews per day, a chart of the most active repositories and a breakdown by repository. Hover a
bar for its numbers, or open **Show as a table**. The administrator's dashboard shows the same
totals and most active repositories for every org, with each day's reviews split by org.

**Metrics.** Each org's **Metrics** tab (for its members and owners) shows, for the last 7, 30 or
90 days or a year, what the bot did and what it caught:

- **Value:** reviews posted, findings (inline and in summaries, per review), P1–P2 findings,
  findings confirmed fixed and the fix rate, the time from the bot first seeing a PR to its first
  review (median and 90th percentile), conversations answered (review-thread replies and
  @mentions), verdicts (approved, changes requested, comment only) and model calls and tokens.
- **Findings per day by level**, and the reviews and findings per day.
- **Repositories:** the most active by reviews posted, findings by repository and level, and a
  table with each repository's reviews, PRs, findings at each level, fixes, fix rate, approvals,
  change requests and failures.
- **Models:** how many reviews were sent to each configured model and how many it answered, its
  replies, calls, failures, tokens and average answer time. With failover, a review is sent to a
  model that fails and then to the next; with collate, every review goes to every model. Members
  see the instance's models only as numbered placeholders ("Instance model 1"), never their names;
  the org's own models show their name and model.

Administrators get the same page for every org together under **Metrics**, plus each day's (or
week's) reviews split by org, each org's findings by level and a table of orgs, and the server and
owner of every model. The split colors up to eight orgs apart; with more, the most active seven
get a color each and the rest share one. Each model's card on the **Models** page shows its
reviews, replies and failed calls over the last 30 days.
Model calls are counted from this version on and kept for 400 days; the other metrics come from
the review history, so they cover earlier reviews too. The fix rate compares the fixes confirmed in
a period with the findings posted in it, so a fix of an older finding counts: read it as an
estimate.

**Per-org review settings.** An org uses the instance's review settings until an owner saves its
**Review settings** page. From then on it has its own: wait after push, comment, approval and
request-changes levels, comment cap, most reviews per PR, drafts, skip labels, guideline and
ignore files, and replies. **Use instance defaults instead** removes them. The skill and the
review queue stay shared.

**An org's own models (bring your own key).** On the org's **Models** page, its owners can add
models with their own API keys, and choose how they're used, as above. As soon as it has one, the
org's reviews and replies use only its own models, never the instance's, and its daily review
limit no longer applies (reviews on its own models don't count towards it). Removing the last one
puts it back on the instance's models and limit. Its models must be served at a public `https://`
address: the bot refuses loopback, private and link-local addresses, also when a host name resolves
to one. Its keys are stored encrypted with a key kept outside the database: `secret.key` in the data
folder, or `PRBOT_SECRET_KEY` (64 hex characters) if set. Back the key up with the database: without
it, orgs have to enter their keys again.

**Sharing the review slots.** The bot runs four jobs at once. One org's jobs may hold at most 50%
of them (two) at a time; its others wait for one of its own to finish, even when no other org has
work. Change the share in **Settings → Sharing the review slots**. If the bot only serves one
org, set it to 100% so all four slots are used.

### Adding more organizations

One bot can review any number of organizations and personal accounts. Each one installs the same
GitHub App, and the bot finds the new installation at its next repository sync (every 15 minutes,
or straight away with **Repositories → Sync from GitHub**).

1. **Let other accounts install the App.** An App set to *Only on this account* can only be
   installed on the account that owns it. On the App's GitHub page, open **Advanced → Make
   public**. Public means any GitHub account *can* install it; it isn't listed in the Marketplace.
   GitHub lets you make it private again only while it's installed on its own account alone.
2. **Limit which accounts are reviewed.** In **Settings → GitHub App**, list the orgs you serve
   under **Only these orgs/accounts**, one per line, *before* you share the install link. While the
   list is empty every installation is reviewed, so anyone who installs the public App gets their
   PRs reviewed on your model (and your API key or ChatGPT plan, with a hosted provider). An
   installation the list leaves out never becomes an organization in the bot: its repositories
   aren't listed, polled or reviewed, and nobody gets access through it. Taking an org that's
   already in the bot off the list pauses it instead; its repositories, settings and history are
   kept.
3. **Send the install link** to an owner of each org. It's on the bot's **Organizations** page
   (**Add an organization → Install the GitHub App**), and is
   `https://github.com/apps/<app-name>/installations/new`. They pick their org and choose all
   repositories or only some. Installing needs an org owner or the *GitHub App manager* role; for
   anyone else, GitHub sends a request that the org's owners approve.
4. **Sync.** Click **Repositories → Sync from GitHub**, or wait for the next sync. New
   repositories are watched straight away while *Watch newly discovered repositories
   automatically* is on (the default). PRs that are already open are marked `baseline`, as in
   [Start reviewing](#5-start-reviewing).
5. **Give people access.** With GitHub sign-in set up, the org's GitHub admins become its owners
   when they sign in, and can add their team (see *Who gets access* above).

To change which repositories are reviewed later, an owner of that org edits the installation on
GitHub (**the org's Settings → GitHub Apps → the App → Configure → Repository access**). The bot
picks up the change at its next sync. Uninstalling the App marks the org *App uninstalled* in the
bot.

**When one bot isn't enough.** Every org shares the skill and the review queue. If an
org must be kept fully separate, give it its own private App and run a
second instance with its own data folder and port:

```powershell
prbot -data C:\prbot\data-other -listen 127.0.0.1:8081
```

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
| OpenRouter | `https://openrouter.ai/api/v1` | Needs an API key from openrouter.ai. Its catalog reports each model's context window and output cap. |
| Anthropic API (Claude) | `https://api.anthropic.com` | Needs an API key from the [Claude Console](https://platform.claude.com/). Uses Anthropic's official Go SDK: streamed answers, the system prompt and tool conversations cached, the JSON schema as structured output, no temperature (current Claude models refuse one). The Models API gives each model's limits. |
| OpenAI API (ChatGPT models) | `https://api.openai.com/v1` | Needs an API key from platform.openai.com, billed to that API account. Sends `max_completion_tokens`; a model that refuses a temperature (reasoning models) is asked again without one. |
| ChatGPT plan | `https://api.openai.com/v1` | No API key: sign in with a ChatGPT account, and reviews use its plan. See [Using a ChatGPT plan](#using-a-chatgpt-plan). |
| Other OpenAI-compatible | e.g. `http://host:8000/v1` | vLLM, llama.cpp server, and others. |

To use a model on another machine, set the base URL to that machine. In LM Studio, enable
*Serve on local network*. For Ollama, start it with `OLLAMA_HOST=0.0.0.0`.

### Using a ChatGPT plan

The **ChatGPT plan** provider uses OpenAI's
[Sign in with ChatGPT plan usage](https://developers.openai.com/siwc/token-sharing-open-source)
(a preview for open-source apps): instead of an API key, a ChatGPT account is signed in and the
model's reviews and replies count against that account's plan.

1. Add a model with the **ChatGPT plan** provider and a model name (after signing in, **Load
   models** lists those the account may use). It takes no key, and its address is always OpenAI's.
2. On its card, click **Sign in with ChatGPT**, then **Open the ChatGPT sign-in**. Sign in and
   allow the use of your ChatGPT plan.
3. The sign-in ends on a `http://127.0.0.1:1455/auth/callback?…` page. Unless the bot runs on the
   computer you're using, that page won't load: that's expected. Copy its whole address from the
   address bar, paste it on the model's card and click **Finish signing in**, within 15 minutes.

The bot then keeps the sign-in and renews its access token as it expires. Each renewal replaces the
refresh token, so renewals of one sign-in run one at a time. Signing out, removing the model or
changing its provider asks OpenAI to end the sign-in, trying again for a few minutes if OpenAI
can't be reached. If OpenAI never confirms it, or the sign-in can't be decrypted (as with an org's
models that can't be read), the org's activity says so; disconnect the app in ChatGPT's settings
to be sure it's ended. A sign-in that didn't allow the plan's use isn't kept, but the next one asks
for it again. Signing a model in again takes the same ChatGPT account: to use another, sign the
model out first.

Things to know:
- Use only your own account. The sign-in is stored encrypted, bound to the set of models it was
  made for (the instance's, or one org's), and only that model uses it.
- The plan's [usage limits](https://chatgpt.com/settings/usage) apply. When they're used up,
  reviews fail with a message that links there; with **Failover**, the next model takes over.
- The preview limits what a request can use
  ([preview limitations](https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations)):
  answers are streamed and not stored at OpenAI, there's no temperature or output cap (the
  **Max output tokens** setting only sizes the bot's prompts), and the repository tools are offered
  inside a namespace. Errors from the preview, such as an account that isn't eligible, are shown as
  OpenAI gives them.

**Why there's no "Sign in with Claude".** Anthropic doesn't permit third-party apps to offer
Claude.ai sign-in or to use Free, Pro, Max or Ultra plan credentials on their users' behalf ([Claude
Code legal and compliance](https://code.claude.com/docs/en/legal-and-compliance)). Use the
**Anthropic API** provider with an API key from the Claude Console instead.

**What works well:** code models of 14B parameters or more with at least a 32k context, for
example Qwen2.5-Coder 32B, Qwen3-Coder 30B-A3B or Devstral. Small general models (around 4B)
produce valid output but tend to summarize the PR and find nothing.

**Reasoning models** (Qwen3, DeepSeek-R1, …) work. Their `<think>` blocks are removed, and reasoning
returned in a separate field is ignored, except that it's sent back with tool calls where a server
needs it (Claude's signed thinking, DeepSeek's thinking mode). However, they spend output tokens on
thinking before answering. If a review fails with *"used all its output tokens on reasoning"*, raise
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
   delay, it's queued. Up to four jobs (reviews or replies) run at once, each on a different PR, so
   the model never gets more than four requests at a time. A PR's own jobs run one after another.
   The dashboard shows the four review slots, each with the PR it's working on or `Idle`.
2. Files that are ignored, deleted or binary are skipped. Each remaining diff line is labelled
   with its new-file line number, so the model doesn't have to work out line numbers inside diff
   hunks. Large diffs are split into parts that fit the context, and the parts' summaries are
   combined at the end. If there's anything to review, the bot first posts a PR comment saying the
   review has started, with the commit, the start time (UTC), the model and the review skill.
   A PR with nothing reviewable gets no such comment. When the review is posted, the comment is
   edited to say so, with a link to it. If the review fails, the comment is edited to say that
   instead (without the error, which is on the PR's page in the bot). It's one comment per PR:
   retries and the reviews of later pushes edit it rather than posting another.
3. Each part gets the diff and, where they fit beside it, the **full contents of its changed
   files** at the head commit. The model also gets **read-only tools** over the repository at the
   head (or base) commit: `read_file`, `search_code` (a grep) and `list_files`. It uses them for
   the skill's context and verification steps: reading definitions, finding callers, and checking
   a finding against the real code before reporting it. The tools only read the PR's own
   repository through the installation's API; nothing is run, and at most 8 calls run per model
   turn. The review's ignore patterns keep files out of the review, not out of reach: a search
   skips them unless the model gives a path pattern, and `read_file` opens any file, so the
   model can still read generated or vendored code for context. Turn them off with *Let the model
   read the repository* in a model's settings: a review offers them only if every model it may
   use has it on. With them on, a quarter of each part's context is kept for what the tools
   return, so parts carry less diff; a model server that refuses tools gets the review without
   them, but its parts stay that size, so turn the setting off for a model without tool support
   to give the diff that room. The prompt tells the model to ignore any limit in the skill on how
   many findings to report.
4. The model returns JSON findings. The bot then asks again about the **same code**, listing
   what's been found so far, until a pass finds nothing new at the *approval level* or above
   (at most 5 passes per part), so one review finds what matters rather than leaving it for later
   pushes. With approvals off, the *request-changes level* decides, and with both off, the
   *comment level*. Less severe findings from any pass are still posted. On an automatic
   re-review, the model is told which lines changed since the bot's last posted review, and only
   findings on those lines (or within 3 lines of them) are kept: the rest of the PR was reviewed
   then. Parts with no such changes aren't sent to the model again. A review asked for with
   **Review now**, or the first one the bot posts on a PR, covers everything. The bot
   checks each finding's file and line against the diff, moving it to the nearest changed line
   up to 3 lines away if needed. It drops findings the bot already raised in an earlier review,
   and findings below the *comment level*. Past the comment cap, the least severe findings are
   **held for the next review** rather than dropped: the review says how many, they count as
   open (so the bot doesn't approve), and the next review checks they still apply, at the line
   they're on now, and posts them with its own findings under the same cap: most severe first,
   and a held finding ahead of a new one at the same level. The last automatic review has no
   next one, so it posts everything.
5. On a re-review, it first checks its unresolved findings on files that changed since the last
   review. For each file it gives the model the current file and the earlier findings, and asks
   which are fixed. For each confirmed fix it **replies in the thread** ("✅ Confirmed fixed in
   `abc1234`…") **and then resolves the thread**. When someone has already answered the finding
   since the bot last spoke in its thread ("Fixed in abc1234"), the thread is resolved without
   the reply: their answer says why. A finding whose file the PR no longer changes is closed the
   same way. Anything the model isn't sure about stays open.
6. It posts **one** GitHub review: inline comments plus a summary. Findings that can't be placed
   on a changed line go in the summary. If GitHub rejects an inline position, the review is
   reposted with every finding in the summary, so nothing is lost.
7. With a *request-changes level* set, the review **requests changes** while a finding at that
   level or above is open: an unresolved earlier thread or a new finding. Otherwise, with an
   *approval level* set, the review is an **approval** when no finding at that level or above is
   open. Anything in between is a comment that says how many findings hold back approval. A later
   comment cancels neither an approval nor a change request on GitHub, so the bot dismisses its
   own: an approval once a finding at the approval level is open again, and a change request once
   nothing at the request-changes level is. Both are re-checked between pushes too. Resolving the
   last open thread, by fix or by hand, gets the PR approved, or lifts the change request. The bot
   doesn't request changes between pushes, though: reopening a thread by hand only withdraws the
   approval, and the next review requests changes again. A reply in the PR triggers the
   check within a poll; a thread resolved by hand with no new comment isn't visible in the PR list,
   so it's picked up by the 15-minute re-check instead.
8. A failed review is retried twice, after 2 and 8 minutes. After that the PR shows `error`
   until the next push or **Review now**. Clicking **Review now** while an automatic review of the
   PR is already queued turns that queued review into the manual one, so the request isn't lost,
   and a manual review that has to wait (for example, when its org's settings can't be read) keeps
   its place even if the queue is full when it's retried.
9. Each posted review counts as one round on the PR, and its body says which ("Bot review number
   3 of 10"). Once the bot has posted *Most reviews per PR* reviews, the next push isn't reviewed:
   the PR shows `limit` and gets one comment saying the limit is reached. Later pushes aren't
   reviewed either, and the comment isn't repeated. Raising the limit gets the latest push reviewed
   as soon as the new limit is saved. **Review now** goes past the limit, and that review counts
   as a round too. Failed reviews, reviews with nothing to review and approvals between pushes
   don't count.
9. Each org also has a **daily review limit**: at most that many posted reviews in any 24 hours
   (100 by default). A review counts from when it started, and reviews under way count too, so
   no two workers can take the last place. A review past the limit isn't run: the PR shows
   `daily` and gets one comment saying the limit is reached and when reviews resume (tried again
   every few minutes if GitHub fails it, and brought up to date if the PR is held again before it
   was reviewed). After that nothing more is reviewed on it, new pushes
   included, and **Review now** is refused for the whole org, until a place frees up 24 hours after the oldest review in the window. Then held
   PRs are let through automatically, longest held first, only as many as there's room for. A
   place freed for a held PR is kept for it until its review starts, so **Review now** on another
   PR can't take it, and a PR held again keeps its place in line.
   Replies in the bot's review threads go on as usual. Only an administrator can change an org's
   limit, under **Organizations → *org* → Settings → Daily review limit** (1 to 100,000, or back
   to the default). Raising it lets held PRs through as soon as it's saved.

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
| `limit` | The bot has posted as many reviews as *Most reviews per PR* allows; new pushes aren't reviewed until the limit is raised or someone clicks **Review now** |
| `daily` | The org has had its daily review limit; the PR is reviewed automatically once there's room |
| `closed` | Closed or merged |

**Conversations.** When someone replies in a thread the bot started, the bot answers in that
thread. It's given the diff hunk, the current code around the line (±30 lines) and the whole
thread (for an outdated comment, ±60 lines). It can agree that an issue is resolved, defend its
comment, or answer the question. When it agrees the concern no longer applies, because the code
now fixes it or the person showed it isn't a real problem, it says so in its reply and then
resolves the thread. A reply posted while a new push waits for its review ("Fixed in
abc1234") is answered after that review: if the review's fix check confirms the fix, the thread
is resolved with no answer, and otherwise the reply is answered against the new code.
When someone writes `@<app-name>` in the PR conversation, the bot answers there, using the
changed-file list, its latest review summary and the recent comments.

**Asking for a review from the PR.** Comment `@<app-name> review` on its own line in the PR
conversation (for example `@omg-review-bot review`), and the bot queues a review just as
**Review now** does: it covers the whole PR, goes past *Most reviews per PR* and ignores the
wait after push, drafts and skip labels, but not the daily review limit. The usual "Queued for
review" comment confirms it. Only people with write access to the repository can ask; anyone
else, or a request the bot can't take (a closed PR, a review already queued or running, the
daily limit reached, reviews stopped for a restart), gets a reply saying why. One that fails for
another reason (GitHub or the bot's database unavailable, a full review queue) is tried again. The command must be a line of its own, so a
question such as "@<app-name> reviewing this, why…?" is answered as a mention, and a quoted
request, or one shown as code (in a code block or between backticks), isn't taken as a new one. Like other mentions, it needs
*Replies / mentions* to have mentions on.

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
| Max comments per review | 20 | The most severe findings are posted; the rest are held for the next review (the last automatic review posts them all) |
| Post comments at | `[P5] NIT` and above | Lower levels are counted in the summary, not posted. Replaces *Minimum severity*: a saved `nit`/`minor`/`major`/`critical` becomes P5/P4/P2/P1. |
| Approve when nothing is unresolved at | `[P3] MINOR (Defect)` and above | A level from P1 to P5, or *Never approve*. Can't be broader than the comment level. For example, comments at P4 with approval at P3: the bot approves when no P1–P3 finding is open, even with open P4 comments. An instance that saved *Never approve* before P3 became the default keeps it. |
| Request changes while anything is unresolved at | `[P3] MINOR (Defect)` and above | A level from P1 to P5, or *Never request changes*. Can't be broader than the approval level, since a finding that requests changes must also hold back approval. With the defaults (both P3), an open P1–P3 finding requests changes and the bot approves when none is open. Make the request-changes level more severe than the approval level, for example P2 with approval at P3, and an open P3 only holds back approval. Saved settings and org policies from before this setting existed get P3 (an org gets the instance's level). |
| Max files per PR | 150 | Remaining files are listed as not reviewed |
| Most reviews per PR | 10 | 5 to 30. How many reviews the bot posts on one PR automatically. After that, new pushes aren't reviewed and the PR gets a comment saying the limit is reached; **Review now** still works. An org policy saved before this setting existed gets the instance's limit. |
| Skip drafts | on | |
| Skip labels | `no-bot-review`, `skip-review` | |
| Project guideline files | `CLAUDE.md`, `AGENTS.md`, root `CONTRIBUTING.md`, style guides … | See [Project guidelines](#project-guidelines). Empty = off. |
| Ignore files | lockfiles, `go.sum`, `vendor/`, `node_modules/`, `dist/`, `*.min.js`, `*.map`, `*.pb.go`, `*.snap`, `*.svg` | gitignore-style patterns |
| Replies / mentions | on / on | Max 5 bot replies per thread. With mentions off, `@<app-name> review` is ignored too. |
| Poll interval | 60 s | Repo list refreshes every 15 min |
| While webhooks arrive, poll every | 900 s | Used instead of *Poll interval* while webhook deliveries arrive (one in the last 24 h) |
| Public URL / Client ID / Client secret / Webhook secret | empty | See [Organizations and sign-in](#organizations-and-sign-in) and [Webhooks](#webhooks) |
| Context tokens / Max output tokens | the model's maximums (131072 / 16384 if its server doesn't say) | Must match how the model is loaded |
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

**Linux (systemd).** The [Linux packages](#linux-packages) install and configure the service
for you. For a manual install under `/opt`, see [deploy/prbot.service](deploy/prbot.service)
for the unit file and install commands. Either way it writes logs to the journal:
`journalctl -u prbot -f`.

**Updating.** First click **Stop reviews** under the review slots on the dashboard. The reviews
and replies under way finish, but no new ones start; due ones wait. Once every slot is idle, the
dashboard says it's safe to restart. Then run
`go install github.com/OMG-Software/omg-review-bot/cmd/prbot@latest` (or rebuild, or install the
newer package) and restart the bot, which starts reviewing again on its own (**Resume reviews**
does it without a restart). Reviews interrupted anyway are re-queued on startup. When
updating from a version without organizations, the database is migrated on startup and each
repository is linked to its org on the next repo sync (**Sync from GitHub** does it at once).
Until then, the activity entries for a repository that isn't linked yet are shown only to
administrators.

## Linux packages

Every push to `main` is built, packaged, installed on each distribution below in CI, and
published to the rolling [`latest` release](https://github.com/OMG-Software/omg-review-bot/releases/tag/latest).
The file names stay the same from build to build, so these links always fetch the newest one.
Each package is for x86_64 or aarch64 (`amd64` / `arm64` for deb):

| Distribution | File |
|---|---|
| RHEL 10 (and Alma, Rocky, CentOS Stream 10) | `prbot-rhel10-x86_64.rpm` |
| Fedora 44 | `prbot-fedora44-x86_64.rpm` |
| Fedora 45 | `prbot-fedora45-x86_64.rpm` |
| openSUSE Leap 16, Tumbleweed | `prbot-opensuse-x86_64.rpm` |
| Debian | `prbot-debian-amd64.deb` |
| Ubuntu | `prbot-ubuntu-amd64.deb` |
| Arch Linux | `prbot-arch-x86_64.pkg.tar.zst` |
| Any other distribution (static binary) | `prbot-linux-amd64.tar.gz` |

Download from `https://github.com/OMG-Software/omg-review-bot/releases/download/latest/<file>`,
then install with your package manager, for example on Fedora:

```bash
curl -LO https://github.com/OMG-Software/omg-review-bot/releases/download/latest/prbot-fedora44-x86_64.rpm
sudo dnf install ./prbot-fedora44-x86_64.rpm
```

The other distributions use `sudo apt install ./prbot-debian-amd64.deb`,
`sudo zypper install --allow-unsigned-rpm ./prbot-opensuse-x86_64.rpm` and
`sudo pacman -U ./prbot-arch-x86_64.pkg.tar.zst`. The packages aren't signed; `SHA256SUMS` in the
release lists their checksums.

A package installs:

| Path | What it is |
|---|---|
| `/usr/bin/prbot` | The bot |
| `/usr/lib/systemd/system/prbot.service` | The service, run as the `prbot` user the package creates |
| `/etc/prbot/prbot.env` | Settings for the service, such as `PRBOT_LISTEN` (kept across upgrades) |
| `/var/lib/prbot` | The data directory, created on first start |

Installing doesn't start the bot. Check `/etc/prbot/prbot.env`, then start it and enable it at
boot:

```bash
sudo systemctl enable --now prbot
```

An upgrade restarts a running bot on the new version; click **Stop reviews** on the dashboard and
wait for the review slots to go idle first, so no review is cut short. Removing the package stops the service but
keeps the `prbot` user and `/var/lib/prbot`, because the database there holds the GitHub App key;
delete them by hand if you're done with the bot. To clear the admin password of a packaged
install, run `sudo -u prbot prbot -data /var/lib/prbot -reset-password`.

The version is in [internal/version/version.go](internal/version/version.go). Packages from the
`latest` release take that version, with the CI run number as the package release (for example
`0.1.0-42`), so each build upgrades the one before. To build the packages yourself, see
[packaging/build-packages.sh](packaging/build-packages.sh).

## Behind a reverse proxy

GitHub sign-in and webhooks need the bot to be reachable over HTTPS. The usual way is to keep the
bot on its default `127.0.0.1:8080` and put a reverse proxy in front of it to handle TLS. Examples
follow for [nginx](#nginx) and [Apache httpd](#apache-httpd); after either, finish with
[Configuring the bot](#configuring-the-bot).

### nginx

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

**Restricting the administrator sign-in (optional).** To allow the password sign-in only from
your own network, add this inside the HTTPS `server` block. Everyone else gets `403` there and can
still sign in with GitHub:

```nginx
location = /admin-login {
    allow 10.0.0.0/8;     # your office or VPN range
    deny  all;
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
}
```

### Apache httpd

These steps use Debian/Ubuntu names (`apache2`, `a2enmod`, `/etc/apache2/sites-available/`). On
RHEL, Fedora and similar, the service is `httpd`, the modules below are loaded by default once the
`mod_ssl` package is installed, and the site file goes in `/etc/httpd/conf.d/`.

**1. Enable the modules** the configuration uses:

```bash
sudo a2enmod ssl proxy proxy_http headers
```

**2. Get a certificate first.** Apache, like nginx, refuses a configuration whose certificate files
don't exist yet, so start with only the port-80 `<VirtualHost>` below, enable the site
(`sudo a2ensite prbot`), reload Apache, and get the certificate (this needs certbot's Apache plugin,
`python3-certbot-apache`):

```bash
sudo certbot certonly --apache -d prbot.example.com
```

**3. Add the HTTPS virtual host** to `/etc/apache2/sites-available/prbot.conf` and reload Apache
(`sudo apachectl configtest && sudo systemctl reload apache2`):

```apache
<VirtualHost *:80>
    ServerName prbot.example.com
    Redirect permanent / https://prbot.example.com/
</VirtualHost>

<VirtualHost *:443>
    ServerName prbot.example.com

    SSLEngine on
    SSLCertificateFile    /etc/letsencrypt/live/prbot.example.com/fullchain.pem
    SSLCertificateKeyFile /etc/letsencrypt/live/prbot.example.com/privkey.pem

    # Keep browsers on HTTPS (see the note on cookies below).
    Header always set Strict-Transport-Security "max-age=31536000"

    # Skill zip uploads can be up to 25 MB, and webhook payloads up to 5 MB. Apache has no 1 MB
    # limit like nginx, but set one explicitly in case a global LimitRequestBody is lower.
    LimitRequestBody 26214400

    # Required: the bot blocks a form post unless the browser's Origin matches this host.
    ProxyPreserveHost On

    # "Test model" waits for the model's reply, up to the LLM timeout (600 s by default).
    ProxyTimeout 600

    ProxyPass        / http://127.0.0.1:8080/
    ProxyPassReverse / http://127.0.0.1:8080/
</VirtualHost>
```

**Restricting the administrator sign-in (optional).** To allow the password sign-in only from
your own network, add this inside the `<VirtualHost *:443>` block. Everyone else gets `403` there
and can still sign in with GitHub:

```apache
<Location "/admin-login">
    # Your office or VPN range.
    Require ip 10.0.0.0/8
</Location>
```

### Configuring the bot

With either proxy in place, in the bot:

1. Open **Settings → Sign-in and webhooks** and set **Public URL** to `https://prbot.example.com`.
   The bot must be at the root of its host name: a path such as `https://example.com/prbot` isn't
   supported, and the setting refuses one. Behind a proxy the bot only sees a plain HTTP
   connection from the proxy, so this is how it knows the address to use for the sign-in callback
   and the webhook URL.
2. Copy the callback and webhook URLs the section shows into the GitHub App, as in
   [Organizations and sign-in](#organizations-and-sign-in) and [Webhooks](#webhooks).

Notes:

- **Keep the `Host` header.** Without `proxy_set_header Host $host` (nginx) or
  `ProxyPreserveHost On` (Apache), the proxy sends `127.0.0.1:8080`, and every save, sign-out and
  button press fails with *cross-origin request blocked*. If nginx listens on a port other than
  443, use `$http_host` instead, which includes the port; Apache's `ProxyPreserveHost` already
  passes the port the browser sent.
- **Cookies.** The bot marks its cookies `Secure` when a request arrives on an `https://`
  **Public URL** (which is why the proxy must pass the `Host` header) or when it terminates TLS
  itself. A direct plain-HTTP visit, such as `http://127.0.0.1:8080` on the server, gets ordinary
  cookies so it still works. The `Strict-Transport-Security` header keeps browsers on HTTPS.
- **Don't expose port 8080.** Keep `-listen 127.0.0.1:8080` so the only way in is through the
  proxy.
- **Renewing the certificate.** certbot's timer renews it; `sudo certbot renew --dry-run` checks
  that renewal works.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| GitHub test fails with `401` or `404` | Wrong App ID (use the number, not the `Iv1…` Client ID), the wrong App's key, or the PEM is missing its `BEGIN`/`END` lines. Also check the PC's clock: the App's sign-in token is time-limited. |
| GitHub test: "isn't installed anywhere yet" | The credentials work; install the App on your org (step 2). |
| No repositories listed | The App isn't installed on them, or the *Only these orgs* setting excludes the org. Click **Sync from GitHub**. |
| An org's PRs aren't reviewed any more | The *Only these orgs* setting excludes it. That pauses the org: its repositories, settings and history are kept and people keep their access, but nothing is polled, reviewed or replied to until it's allowed again. |
| An open PR is never reviewed | It's probably `baseline` (it was open before the repo was watched). Push to it or click **Review now**. |
| Reviews post with zero findings | Open the review's **Raw model output**. An empty `"comments": []` means the model found nothing, which small models often do, so try a larger code model. If the raw output has comments that weren't posted, they were filtered as duplicates or below the comment level. |
| Fixed findings aren't resolved, or the bot never approves | Thread states come from GitHub's GraphQL API. Check the activity log for "could not read review thread states". While they can't be read, every finding counts as open, so the bot doesn't approve. The model's fix checks are in the review's raw output under `fix check:`. |
| "could not withdraw approval" or "could not withdraw change request" | The base branch's protection limits who can dismiss reviews. Allow the App to dismiss reviews, or dismiss the review by hand. |
| "used all its output tokens on reasoning" | Reasoning model; raise **Max output tokens** or switch model. |
| "model context … is too small" | Raise **Context tokens** (and the model's loaded context), or shorten the skill. |
| "did not return valid JSON" | Turn on **Enforce JSON schema output**. If your server rejects it, use a stronger model. |
| Forgot the admin password | Stop the bot and run `prbot -reset-password`. |
| The sign-in page has no password field | It's on its own page: go to `/admin-login`. See [Administrator sign-in](#administrator-sign-in). |
| GitHub sign-in: "redirect_uri is not associated with this application" | The App's **Callback URL** doesn't match. Copy it from **Settings → Sign-in and webhooks**, and set **Public URL** if the bot is behind a proxy. |
| Signed in but no organizations listed | The App isn't installed on your org, or you're a plain member and the org hasn't let members in: ask an owner to add you. |
| "Couldn't read your role in …" | Give the App the **Members: Read-only** organization permission, then accept the new permission on the org's installation page. Until GitHub can confirm someone's role, access that came from GitHub is paused; access added by hand isn't affected. |
| Behind a proxy, every save or button fails with "cross-origin request blocked" | The proxy isn't passing the original `Host` header. See [Behind a reverse proxy](#behind-a-reverse-proxy). |
| Webhook deliveries fail with `413` | The proxy's request size limit is too small; set `client_max_body_size 25m` in nginx, or `LimitRequestBody 26214400` in Apache. |
| Webhook deliveries fail with `401` | The secret in GitHub differs from the bot's. Set them to the same value. |

## Security

To report a vulnerability, see [SECURITY.md](SECURITY.md). Please report it privately, not in
a public issue.

- The UI listens on `127.0.0.1` only by default. Until an admin password is set, anyone who can
  open the UI can set one, so set it right away if you change `-listen`.
- The GitHub App private key and the instance's model API keys are stored unencrypted in
  `data/prbot.db`. Keep the `data` folder private; it's gitignored. Orgs' own models and every
  ChatGPT sign-in are stored encrypted (see `secret.key` above); a sign-in only decrypts for the set
  of models it was made for, so one org's model can't use another's.
- An Anthropic API model without a key of its own sends none: the bot never uses the host's
  `ANTHROPIC_API_KEY`, `ANTHROPIC_BASE_URL` or other Anthropic environment settings.
- A ChatGPT sign-in uses an authorization code with PKCE. The verifier never goes through the
  browser: the bot sends it only to OpenAI's token endpoint. The pasted address is checked against
  the sign-in it started, and its ID token against OpenAI's published keys, issuer, audience,
  expiry and nonce. Its tokens go only to OpenAI.
- The bot's only GitHub actions are posting reviews, review replies and PR comments. The App
  permissions above are the minimum it needs.
- PR titles, descriptions and code are untrusted. They're given to the model as data, and the
  prompts tell it to ignore instructions inside them. If an injected instruction got through
  anyway, the worst outcome is a misleading comment: the model's only tools read files of the
  PR's own repository (at its head or base commit) through the bot's installation, so it can't
  run anything, write anything or reach another repository or host.
- Everything the model writes (findings, summaries, thread replies, `@<app-name>` answers and
  fix confirmations) is made inert before the bot posts it under its own name. Raw HTML is
  escaped and Markdown images become plain links, so nothing loads (or leaks data in its URL)
  just by viewing the PR. `@mentions` are broken, so the bot can't be made to notify people or
  command other bots, and so are lines starting with a slash command (`/approve`, `/merge`), for
  chat-ops bots that trust its account. Fake copies of the bot's own hidden markers are broken
  up, so they can't change what it thinks it posted. Each is broken with an invisible zero-width
  space. Code blocks are left as they are, apart from those markers and lines that are a bare
  slash command; a suggested change that contains either is posted as plain code, so applying
  it can never commit the invisible character.
- Requests carrying the GitHub token or the model API key go only to the configured host: a
  redirect elsewhere is refused, as is a GitHub pagination link to another host. The model
  **Base URL** must be `http://` or `https://`. Repository files are only read by paths without
  empty, `.` or `..` segments.
- The UI uses an HMAC-signed session cookie and PBKDF2-hashed password. It rejects
  cross-origin POSTs (CSRF) and sends a strict Content-Security-Policy.
- GitHub sign-in uses a one-time `state` check. The bot keeps each person's GitHub ID, login,
  name and avatar, never their GitHub token. A person sees only the orgs they belong to; asking
  for another org's page, PR or review returns `404`.
- **Sign out everywhere** (Account page) ends all of a GitHub user's sessions, and blocking a user
  ends theirs at once. Changing the administrator password signs everyone out.
- If sign-in can't update someone's access, their sessions end. Should neither the database
  nor the data directory accept that, the bot keeps them out in memory and retries. So that a
  crash can't let them back in, a bot that didn't stop cleanly signs everyone out when it
  starts (it keeps a `running` file in the data directory while it runs).
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

CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) checks formatting, vets, builds and runs
the tests with the race detector on Linux amd64 and arm64 for every pull request and push to
`main`. [.github/workflows/release.yml](.github/workflows/release.yml) builds the packages; it
also runs on pull requests that change `packaging/` or `deploy/`, without publishing. To release
a new version, change `Version` in `internal/version/version.go`.

| Package | Responsibility |
|---|---|
| `cmd/prbot` | Entry point, flags, Windows service / signal handling |
| `internal/bot` | Polling, webhook events (`webhook.go`), the job queue and worker, reviews, replies, prompts, fix checks and approval (`resolve.go`), ignore-file patterns (`filter.go`), project guidelines (`guidelines.go`) and making model output safe to post (`sanitize.go`) |
| `internal/github` | GitHub App auth (JWT, installation tokens), the REST calls used, GraphQL for review threads, and sign-in and webhook signatures (`oauth.go`) |
| `internal/llm` | OpenAI-compatible (OpenAI API, OpenRouter and others), native Ollama, Anthropic (official SDK) and ChatGPT plan (Responses API) clients, JSON extraction |
| `internal/chatgpt` | Sign in with ChatGPT: authorization code with PKCE, ID token checks, token renewal and revocation |
| `internal/skill` | `SKILL.md` parsing, zip install, language-aware reference selection |
| `internal/diff` | Unified-diff parsing and line-numbered rendering |
| `internal/store` | SQLite schema, migrations and queries; users, orgs, memberships, deliveries and statistics (`tenancy.go`) |
| `internal/settings` | Settings model, defaults and per-org review policy |
| `internal/web` | Web UI: handlers, sessions (`auth.go`), GitHub sign-in (`oauth.go`), org pages (`orgs.go`), admin pages and the webhook endpoint (`admin.go`), SVG charts (`charts.go`), the on-disk note of sessions to end after a restart (`pending.go`), embedded templates and static files |
| `internal/textutil` | Small string helpers shared across packages |
| `internal/version` | The release version, `-version` output and the GitHub User-Agent |
| `packaging` | nfpm config, systemd unit, sysusers file and install scripts for the Linux packages |

`internal/bot/bot_test.go` runs the bot end to end against fake GitHub and fake model servers.
It's the best place to start when changing review or reply behaviour.
`internal/web/multiuser_test.go` covers GitHub sign-in, org access and webhook deliveries against
a fake GitHub.

## License

Apache 2.0. See [LICENSE](LICENSE).
