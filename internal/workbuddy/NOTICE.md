# WorkBuddy compatibility references

The whole-conversation fingerprint sanitization in
`runtime/channel_fingerprint.go` adapts ideas from
[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel),
specifically its `internal/upstream/sanitize.go`, copyright 2026
linguo2625469 and copyright 2026 Sliverkiss (original project
`Sliverkiss/workbuddy2api`), under the MIT license reproduced in `LICENSE`.

That project replaces the client system prompt with a gateway-owned one. This
gateway does not adopt that approach, because it discards client skills, tools
and permission instructions. Only the rewrite table and the observation that
the same client templates can surface in history, reasoning and tool payloads
were carried over; the implementation here is confined to the deepest
compatibility tier and leaves everything outside the verified templates
untouched.

Gateway orchestration, authorization, account pool and management remain
work2api code.