# 🛡️ 依赖安全漏洞巡检 MCP Server (OSV.dev Vulnerability Auditor)

基于 **Google OSV.dev** 统一开源漏洞数据库开发的轻量级安全巡检 MCP 服务。支持自动解析并审查多种语言技术栈的第三方依赖库风险。

## 🌟 支持的语言与生态

| 语言/生态 | 识别的清单文件 | OSV Ecosystem |
| :--- | :--- | :--- |
| **Go** | `go.mod` | `Go` |
| **Node / Web** | `package.json`, `package-lock.json` | `npm` |
| **Android / Java** | `gradle/libs.versions.toml`, `build.gradle.kts` | `Maven` |
| **Python** | `requirements.txt` | `PyPI` |

---

## 🚀 提供的 MCP Tools

1. **`audit_workspace_vulnerabilities`**:
   - 自动遍历项目工作区，提取所有模块的依赖版本，批量向 OSV.dev 请求比对，生成完整的 Markdown 漏洞风险评估报告。
2. **`check_package_vulnerability`**:
   - 查询指定组件名与版本的已知安全漏洞（含 CVE、GHSA、严重度评分、修复版本建议）。
3. **`query_vulnerability_details`**:
   - 输入 CVE 编号或 GHSA 编号，获取其在 OSV.dev 上的官方技术细节、受影响版本范围与补丁 commit。

---

## ⚙️ 如何配置为 MCP Server

在任何支持 MCP 的宿主（如 DeepSeek Harness、Claude Desktop、Cursor 等）的配置文件中添加：

```json
{
  "mcpServers": {
    "vuln-auditor": {
      "command": "python",
      "args": [
        "D:\\Projects\\home-datacenter\\tools\\vuln-scanner\\server.py"
      ]
    }
  }
}
```

---

## 💻 命令行直接运行

你也可以随时脱离 MCP，直接在终端中一键扫描当前项目或指定目录：

```powershell
python D:\Projects\home-datacenter\tools\vuln-scanner\scanner.py D:\Projects
```
