# Remote workspace architecture course

This course is for developers reviewing wanctl or integrating an AI host. Its goal is to explain how a remote workspace works and judge its design and evidence. You do not need to begin with Go, process groups or network packets.

## Read breadth first

Start with the whole system, then its boundaries and state, and finally workflows, failures, evidence and integration choices. Each lesson focuses on one architectural judgment, with a diagram, an example and an optional self-check.

| Order | Lesson | Question you can answer |
| --- | --- | --- |
| 01 | [System map](https://wc.z10.dev/docs/learn-system-map/) | Which component owns each part of a remote change? |
| 02 | [Integration boundaries](https://wc.z10.dev/docs/learn-integration-boundaries/) | Which tools can MCP cover, and where must the host participate? |
| 03 | [State and lifetimes](https://wc.z10.dev/docs/learn-state-and-lifetime/) | What is lost after a disconnect or a process restart? |
| 04 | [Working with results](https://wc.z10.dev/docs/learn-workflows/) | How do references, hashes, request IDs and offsets connect operations? |
| 05 | [Failures and authority](https://wc.z10.dev/docs/learn-failures-and-authority/) | What should happen first after an uncertain result or refusal? |
| 06 | [Evidence and delivery](https://wc.z10.dev/docs/learn-delivery-and-evidence/) | Which claim does a test result actually support? |
| 07 | [CLI, MCP and OAuth](https://wc.z10.dev/docs/learn-cli-mcp-oauth/) | How should a host retain authorization and workspace bindings? |

## References to return to

- [Architecture terms](https://wc.z10.dev/docs/learn-terms/): consistent meanings for host, controller, workspace, authorization and request ID.
- [Printable architecture card](https://wc.z10.dev/docs/learn-architecture-card/): six questions for review and recall.
- [Usage contract](https://github.com/Daily-AC/wanctl/blob/v0.12.1/docs/workspaces.md): consult this when you need parameters, limits and operations.
- [Primary sources](https://github.com/Daily-AC/wanctl/blob/main/docs/learning/remote-workspace/RESOURCES.md): distinguish protocol facts from project decisions and continue reading.

## How to use the course

Lesson bodies are currently in Chinese, with v0.12.1 as their implementation baseline. Diagrams are architectural models; implemented capabilities, boundaries and possible extensions are distinguished. Self-checks give feedback on the page without uploading answers or recording personal scores.

Try explaining the idea in plain words, then compare your explanation with the diagram and sources. Bring a concrete question to your collaborating agent and ask for an explanation at the same level before diving into code. Reading and self-checks are not access requirements.
