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

- **Checked 2 Nov 2026 against OpenCode's docs:** Go's endpoints are under
  `https://opencode.ai/inference/go/` (for example
  `openai/v1/chat/completions` for DeepSeek, GLM and Kimi, and
  `anthropic/v1/messages` for Qwen and MiniMax). Requests need an
  `x-opencode-session` header (a stable id per conversation) and a client user
  agent. Polymux's `POLYMUX_MODEL_URL` still names the older
  `https://opencode.ai/zen/go/v1`. Jev is not on the Go plan: it is on the
  Console (Zen) API at `https://opencode.ai/inference/systemone/v1/systemone`
  (`jev-1.13`, or `jev-1.13-free` for a limited time), which `--provider
  opencode` uses. The key opencode saved for `opencode-go` on this Mac is
  rejected ("Invalid credential"); the key in `POLYMUX_MODEL_KEY` works. With
  it, `jev-1.13` needs Console funds (HTTP 402 on a Go-only account) and
  `jev-1.13-free` answers: pass `OPENCODE_API_KEY` and
  `ARGUS_JEV_MODEL=jev-1.13-free`.
- **Open item (tracked; see the Open items list in RUN.md section 5):** a chat-model provider for OpenCode Go itself.
  OpenCode keeps the Go credential in its auth store, but
  `loadOpenCodeProviderKey` only reads `opencode.json` provider options, so the
  key is not picked up automatically. A native `--provider opencodego` should
  wait until both are confirmed.
- Runs whose provider manifest doesn't show OpenCode Go are not counted as
  Polymux test evidence.

## Reference account: UniMelb student Grok Bot

- The Grok Bot reference for Polymux testing is the **UniMelb student
  account's Grok Bot** (the student account). It is not the Outlook account and not the main account.
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
