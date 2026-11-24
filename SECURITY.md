# Security policy

## Reporting a vulnerability

Please don't open a public issue, pull request or discussion for a security problem.

Report it privately through GitHub instead:

1. Go to the repository's [**Security** tab](https://github.com/OMG-Software/omg-review-bot/security).
2. Choose **Report a vulnerability**.
3. Fill in the form. Only you and the maintainers can see it.

It helps if you include:

- what an attacker can do, and what they need first (an account, a pull request in an installed
  org, access to the UI, access to the server);
- the version (`prbot -version` prints it and the commit it was built from) and whether you're
  on Windows or Linux;
- steps or a proof of concept that shows it;
- any fix you'd suggest.

We'll reply on the report, keep you updated while we work on a fix, and credit you in the
advisory unless you'd rather we didn't. Please give us a chance to release a fix before you
talk about the problem in public.

## Supported versions

Fixes go into `main` and ship in the next build. Only the newest version gets security fixes,
so update to it before reporting if you can.

| Version                    | Supported |
| -------------------------- | --------- |
| Latest build from `main`   | Yes       |
| Anything older             | No        |

## What counts

The bot reviews untrusted code with a model, so these are the reports we most want:

- a pull request, comment or mention that makes the bot do something other than post a
  review, reply or comment, or post something that isn't inert (live HTML, images that load,
  working `@mentions` or slash commands);
- the GitHub token, App private key, client secret, webhook secret or model API key reaching
  any host other than the one it belongs to, or showing up in a page, log or comment;
- someone seeing or changing another organization's settings, pull requests, reviews or
  activity, or acting as an administrator without the password;
- getting around sign-in, sessions, sign-out, blocking, CSRF checks or the
  Content-Security-Policy;
- reading or writing files outside the repository paths the bot is meant to read, or outside
  its data directory.

These aren't vulnerabilities on their own:

- the model writing a wrong or misleading review comment, including because a pull request
  told it to. The model has no tools, so that's the expected worst case;
- problems that need someone who can already read the `data` directory or change the server's
  settings;
- an instance whose UI was opened to the network (`-listen`) before an administrator password
  was set.

The [Security section of the README](README.md#security) explains how the bot protects
itself.
