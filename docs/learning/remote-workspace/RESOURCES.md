# 远程工作区架构资料

课程以 wanctl v0.12.1 为实现基线，MCP 2025-06-18 为核心协议参考。以下资料分别支持一般协议事实与项目自身的设计选择；不要把 wanctl 的行为写成所有 MCP 实现都必须如此。

## Knowledge

- [MCP Architecture](https://modelcontextprotocol.io/specification/2025-06-18/architecture)：宿主、客户端与服务端的职责边界，用于第一、二篇。
- [MCP Transports](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports)：stdio、HTTP 请求与协议会话的区别，用于第三、七篇。不要从协议推断某个产品一定怎样重建进程。
- [MCP Tools](https://modelcontextprotocol.io/specification/2025-06-18/server/tools)：工具声明、结构化返回及用户审批的协议背景，用于第二、四、六篇。
- [MCP Authorization](https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization)：HTTP 授权与传输的关系，用于第七篇。
- [GNU Bash: Command Execution Environment](https://www.gnu.org/software/bash/manual/html_node/Command-Execution-Environment.html)：执行环境继承与子 shell 的边界，用于第三篇。
- [Python subprocess](https://docs.python.org/3/library/subprocess.html#subprocess.run)：新程序进程的启动与完成，用于区分脚本执行和持久 Python 内核。
- [OAuth 2.0, RFC 6749 §1.4](https://www.rfc-editor.org/rfc/rfc6749#section-1.4)：访问令牌表示授权；不把令牌误称为工作区状态。
- [OAuth Security BCP, RFC 9700](https://www.rfc-editor.org/rfc/rfc9700.html)：需要深入授权实现时使用，初读课程不要求掌握全部安全细节。
- [JSON-RPC Request Object](https://www.jsonrpc.org/specification#request_object)：消息 ID 的通信用途，用于第五篇的编号区分。
- [VS Code Remote Extensions](https://code.visualstudio.com/api/advanced-topics/remote-extensions#architecture-and-extension-kinds)：UI 与工作区扩展的运行位置，用于第二篇的架构对照。
- [wanctl 工作区契约](https://github.com/Daily-AC/wanctl/blob/v0.12.1/docs/workspaces.md)：本项目的状态归属、操作、限制和身份边界。
- [wanctl 输出结果契约](https://github.com/Daily-AC/wanctl/blob/v0.12.1/docs/mcp-output.md)：本版的 state/done/code、摘要和偏移语义。
- [设备端工作区实现](https://github.com/Daily-AC/wanctl/blob/v0.12.1/internal/agent/workspace.go)：核验 shell、账本、去重、取消与资源边界。
- [HTTP 与 stdio 集成测试](https://github.com/Daily-AC/wanctl/blob/v0.12.1/internal/mcp/workspace_test.go)：对照第六篇，区分接口、真实执行与宿主使用证据。

## Wisdom (Communities)

- [wanctl Issues](https://github.com/Daily-AC/wanctl/issues)：带具体宿主、操作步骤、预期和实际结果讨论项目问题；不要公开令牌或设备凭据。不要求读者必须参与。
- [MCP Discussions](https://github.com/modelcontextprotocol/modelcontextprotocol/discussions)：协议边界或兼容性问题的上游讨论入口；先区分项目实现与协议要求。

## Gaps

不同网页宿主的审批判据和对话到 MCP 会话的映射可能变化，公开规范不能替代现场验证。某次工具被拒绝而没有详细原因时，课程只记录观测，不推断隐藏规则。公开课程不记录个人学习成绩；目前也没有足够证据给出“学习后已掌握”的结论。
