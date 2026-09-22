# First-contact trust approval diagnosis, 2026-09-22

## Observed cause

The ChatGPT Workspace Preview connection inherited **Allow low-risk actions**.
The established wanctl connection instead had **Allow all actions**. Plugin
Management independently reported both settings. The preview's mode can deny a
sensitive action automatically rather than present a human confirmation.

After the owner explicitly selected **Allow read actions / ask before writes**
for the preview only, the same first-contact trust request produced an approval
card. The card identified **suspicious instructions**, explaining:

> 工具描述试图规定信任指纹的调用时机、跳过用户确认并指定分类结果相关行为

This is direct UI evidence, beyond the previous generic safety-check error.
The old tool description told the model not to ask the user, and the first-
contact response said `DO THIS NOW, without asking the user first`. They also
asserted that the MCP host already asks for every tool call. That assumption is
false for the auto-review permission mode.

The preview's pinning feature was enabled, and the device certificate still
matched the independently verified fingerprint. These were checked through the
operator's existing device/SSH path without exposing credentials. OAuth login
and tool discovery were already working. The immediate barrier was ChatGPT's
approval layer evaluating the tool instructions.

## Fix

The first-contact response and related catalog descriptions now explain the
trust-store mutation and defer to the user's existing authorization and the
host's approval requirements. They no longer direct the model to skip user
confirmation or assume how a host implements approvals. The runtime response
states that it is not authorization and that denied approvals must not be
bypassed. Deployment-specific pinning availability is described conditionally.

No tool was renamed, hidden or labelled read-only. The tool schema, handler,
operator opt-in, certificate mismatch rejection and device policy are unchanged.
The old pending request was explicitly declined, leaving the preview's device
unpinned while the corrected description was prepared.

The owner-authorized permission change is limited to Workspace Preview:
`ask_before_writes`. The normal wanctl connection and global default were not
changed. This preserves explicit human approval for trust writes.

## Verification

The existing trust-flow tests now require truthful mutation and approval
semantics and reject the removed bypass wording. Full standard and lark suites
passed (32 tested packages each); both vet runs, lark build and Windows/Android
cross-builds passed. The full suite also caught stale delegated-instruction
source references left by the earlier workspace help update; those references
were synchronized without changing the delegated protocol or its permissions.

The candidate binary is deployed only to the isolated preview MCP service.
The browser connector must refresh its tool definitions before retesting;
server deployment alone does not update ChatGPT's cached description.

Local evidence and the final browser comparison are retained under
`artifacts/trust-diagnosis-20260922/`. The broader remote-workspace development
acceptance remains separate from a successful approval-card diagnosis.

## Relevant official guidance

OpenAI's [plugin security guidance](https://developers.openai.com/plugins/guides/security-privacy)
describes explicit consent, host confirmation, and reviewing tool descriptions
for prompt-injection risks. [Tool annotations](https://developers.openai.com/plugins/reference#annotations)
are hints for presenting operations, not substitutes for server authorization.
The observed approval-card text, rather than these general documents, identifies
the specific reason displayed for this request.
