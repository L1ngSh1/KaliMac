package cli

import (
	"fmt"
	"io"
)

var commandHelpText = map[string]string{
	"init": `km init — 初始化或恢复当前项目环境

用法:
  km init [--image IMAGE] [--platform OS/ARCH]

说明:
  新项目默认写入 .km.json，并创建当前项目专属环境。
  已有配置上的参数只能确认一致，不能隐式覆盖或重建。
  创建结果不确定时按项目身份核实，不自动重试创建。
`,
	"status": `km status — 查看当前项目状态

用法:
  km status [--json]

说明:
  文本模式适合人工查看；--json 提供稳定的结构化状态。
  查询失败会报告 unknown，不会解释为资源不存在。
`,
	"doctor": `km doctor — 深入检查平台、运行时、项目和容器身份

用法:
  km doctor

说明:
  doctor 是只读诊断。命令完成不等于环境健康；请查看结论中的失败数。
`,
	"tools": `km tools — 查看精选工具在项目环境中的可用性

用法:
  km tools

说明:
  AVAILABLE 仅表示工具可从 PATH 定位；本命令不运行工具或查询其版本。
`,
	"run": `km run — 使用长形式执行工具

用法:
  km run -- TOOL [ARG...]

说明:
  -- 是必需分隔符；之后的参数原样传给工具。
`,
	"shell": `km shell — 进入当前项目的交互 shell

用法:
  km shell

说明:
  需要真实终端。异常中断后使用 km sessions 和 km cancel <id> 恢复。
`,
	"stop": `km stop — 停止当前项目环境

用法:
  km stop

说明:
  操作幂等，项目文件和本地状态保留。
`,
	"sessions": `km sessions — 查看当前项目会话

用法:
  km sessions

说明:
  只读列出活跃和遗留会话；完整 ID 可交给 km cancel。
`,
	"cancel": `km cancel — 取消指定会话

用法:
  km cancel <id>

说明:
  仅操作当前项目中由 km sessions 返回的完整会话 ID。
`,
	"version": `km version — 显示版本与构建身份

用法:
  km version
  km version --verbose

说明:
  详细模式显示版本、源码提交、工作区标记和目标平台；不访问 Docker。
`,
}

// PrintCommandHelp writes one management command's help and reports whether
// the name is known. Tool names never pass through this function.
func PrintCommandHelp(w io.Writer, command string) bool {
	text, ok := commandHelpText[command]
	if !ok {
		return false
	}
	fmt.Fprint(w, text)
	return true
}
