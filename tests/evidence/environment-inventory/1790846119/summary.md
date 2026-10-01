# 环境资源查看与显式清理：执行证据（run 1790846119）

- 分支：codex/environment-inventory（基于 c9f297b）；平台 darwin/arm64 本机实测
- Docker Engine 29.6.1

## 验证命令与结果

| 命令 | 结果 |
| --- | --- |
| gofmt -l internal/ tests/（本轮改动） | 干净 |
| go vet ./... / go vet -tags=integration ./... | PASS |
| go test -count=1 ./... / go test -race -count=1 ./... | PASS |
| go build ./... | PASS |
| go test -tags=integration -count=1 -timeout 15m -v ./tests/integration/ | 66 PASS / 0 FAIL / 0 SKIP，零残留（含新增 TestEnvRemoveRealDrillABC 与 TestEnvRemoveRealKill9） |
| tests/acceptance/install_env_switch_acceptance.sh（扩展 list/remove 后） | 31/31 PASS |
| 旧二进制 kind=remove 门禁实测 | 旧 fail-closed（kind 非法）/ 新识别 pending（old-binary-remove-gate.txt） |

## 资源账本

- 引擎核验：km.owner=km 容器残留 = 0
- 删除演练的可写层金丝雀随容器删除消失；项目文件金丝雀保留；镜像未删

## 未验证/边界

- Intel (amd64) 实机未做（本机 arm64；amd64 为交叉编译产物）
- 真实 kill 落点存在观测竞态（以恢复不变量为验收标准；remove 场景真实命中 SIGKILL）
