# Polymux testing: model and Grok Bot reference (13 Oct 2026)

These rules apply to every Argus session that tests Polymux and to any
model-driven test agent running inside Polymux.

## Model: OpenCode Go subscription

- Polymux testing uses the **OpenCode Go** subscription as its model. On cvmini,
  `opencode auth list` shows it as `OpenCode Go`, and the models are listed under
  the provider id `opencode-go` (for example `opencode-go/deepseek-v4.1-flash`
  and `opencode-go/deepseek-v4-flash-vision-exp`).
- Argus has no native `opencode-go` provider yet. Use the OpenAI-compatible
  path:

  ```sh
  ARGUS_PROVIDER=commandcode \
  ARGUS_COMPAT_BASE_URL=<OpenCode Go OpenAI-compatible base URL> \
  ARGUS_COMPAT_MODEL=deepseek-v4.1-flash \
  ARGUS_VISION_MODEL=deepseek-v4-flash-vision-exp \
  ARGUS_COMPAT_API_KEY=<read from the vault at run time; never write it to a file or log> \
  argus ...
  ```

- **Open item (tracked; see the Open items list in RUN.md section 5):** confirm the OpenCode Go base URL and how the key is supplied.
  OpenCode keeps the Go credential in its auth store, but
  `loadOpenCodeProviderKey` only reads `opencode.json` provider options, so the
  key is not picked up automatically. A native `--provider opencodego` should
  wait until both are confirmed.
- Runs whose provider manifest doesn't show OpenCode Go are not counted as
  Polymux test evidence.

## Reference account: UniMelb student Grok Bot

- The Grok Bot reference for Polymux testing is the **UniMelb student
  account's Grok Bot** (carlvincet@student.unimelb.edu.au). It is not the Outlook account and not the main account.
- **Cleanup is part of every test session (required).** When an Argus session
  ends, pass or fail, delete every test conversation, agent and artifact that
  the session created on that Grok Bot. Delete only what the session created;
  never touch pre-existing items. Record what was cleaned up, or that
  nothing was created, in the run report. A session without that record is
  incomplete.
- Test sessions use only the student account. Never the main account's logins,
  keys or keychain.
- Run the student account in a separate browser profile, never the main
  account's profile.
