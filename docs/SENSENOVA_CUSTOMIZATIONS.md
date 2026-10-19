# 商汤兼容配置与升级维护

本 Fork 保留商汤兼容能力与调度修复，基于官方 `0.2.17` 迁移。

## 客户端协议跟随

对 OpenAI API Key 账号设置以下 `extra`：

```json
{
  "openai_follow_client_protocol": true,
  "openai_strip_reasoning_summary": true
}
```

启用 `openai_follow_client_protocol` 后，Chat Completions 请求保持 Chat Completions 上游，Responses 请求使用原生 Responses 上游。Messages 入口继续使用账号原有兼容路径。

Responses 路由在当前请求的账号副本上覆盖，不修改调度缓存共享的账号配置；关闭此开关后恢复官方的探测及手动覆盖逻辑。两项开关都保留在调度缓存投影中，重启或缓存刷新不会使它们失效。

## 仅过滤请求中的 reasoning.summary

`openai_strip_reasoning_summary` 必须是布尔值 `true`，默认不启用。过滤在普通 Responses、透传 Responses 和共用 Chat Completions 的最终 HTTP 出站点执行，避免后续转换重新添加字段。

只删除请求选项 `reasoning.summary`，保留 `reasoning.effort`、历史 reasoning item 的 summary、其他同名字段及响应中的思考内容。不支持摘要控制参数的上游可以开启；其他上游维持原行为。

## 检测完成后等待 5 小时

原生账号定时测试支持 `@every 5h` 等标准间隔表达式，并继续接受五字段 Cron。与小时字段 `*/5` 不同，间隔表达式不会在跨午夜时缩短为 4 小时。

每个计划设置 `enabled=true`、`auto_recover=true`。原生执行器每分钟扫描一次，实际触发可能晚不足一分钟；检测成功后沿用官方自动恢复逻辑。账号需允许调度，异常状态仍会阻止它参与正常请求，直至检测成功恢复。人工关闭调度的账号不会因本补丁自动打开。

账号、分组、模型名单、限流规则及检测计划均保存于数据库，不写入源码。账号额度是否可用仍由对应模型的上游响应决定。

## 429 后在中转站内切换账号

启用官方高级调度器，按近期成功率、失败率、并发负载及排队情况评分。已知模型冷却在评分前排除；首选 Top-K 账号因缓存延迟在数据库复查时被排除后，继续检查同分组后备账号，不提前报告无可用账号。扩展候选沿用每次选号最多 64 次抢槽、64 次数据库复查的共享预算，取消、权限、分组和模型能力检查继续生效。

线上使用以下数据库设置；这是运行配置，不是上游额度或可用性的保证：

```json
{
  "openai_advanced_scheduler_enabled": "true",
  "openai_advanced_scheduler_lb_top_k": "2",
  "openai_advanced_scheduler_weight_error_rate": "8",
  "openai_advanced_scheduler_weight_priority": "1",
  "openai_advanced_scheduler_weight_load": "2",
  "openai_advanced_scheduler_weight_queue": "1"
}
```

Compose 为应用设置 `GATEWAY_MAX_ACCOUNT_SWITCHES=34`，一次请求最多尝试 35 个不同账号，覆盖当前 35 个非重复有效账号的规模；实际候选仍限于调用方分组，失败账号在该请求内排除。只在尚未输出内容时切换，工具调用不会因流中途重放而重复执行。所有同组账号均被上游限流时仍应返回错误，不能用无限重试或跨组借用账号掩盖容量不足。

数据库设置和 Compose 环境变量会随数据卷与部署配置保留；Top-K 后备修复需要继续使用包含本 Fork 提交的镜像。

## 后续同步官方版本

1. 在本 Fork 的定制分支上合并选定的官方版本，逐项核对以上能力；官方已完整实现的部分不再重复保留补丁。
2. 运行现有后端检查，并在项目外验证协议跟随、账号副本隔离、摘要字段过滤、工具调用及间隔跨日计算。新测试及产物放在 `~/.codex/tools/sub2api/<任务>/`，不加入项目测试目录。
3. 从合并后的源码重建前端及后端，构建带独立版本标签的定制镜像。部署版本应对应可追溯的 Git 提交。
4. 升级前备份数据库、Compose，并保留旧镜像；先在数据库副本验证迁移，再替换线上应用，保留原数据库卷。
5. 切换后核对账号与分组、模型列表、Chat、Responses、工具调用及定时任务的下次执行时间。

官方镜像或官方二进制升级不包含本 Fork 的未合并修改。切换到其他版本前必须确认其支持 `@every`：不支持时，下次执行时间计算会失败，未推进的到期计划可能在后续扫描中重复执行。
