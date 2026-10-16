# 仪表板指南

> 简体中文 | [English](dashboard.md)

本文件说明 `token-usage serve` 提供的内嵌仪表板：如何启动、仪表盘页与配置页的行为，以及背后的 HTTP 接口。行为描述以源码实现为准。

## 启动仪表板

仪表板始终以后台方式运行，由 `serve` 命令组管理：

```bash
token-usage serve start --open
```

服务默认监听 `127.0.0.1:8619`（`--addr` 修改监听地址），`--open` 在后台服务就绪后用默认浏览器打开页面。`serve status`、`serve stop`、`serve restart`、状态文件、日志与单实例合同见 [CLI 参考的 serve 节](cli.zh-CN.md#serve)。

## 安全边界

HTTP 面按回环与同源使用（不设 CORS 头）、无鉴权：数据接口与页面本身严格只读；唯一写路径是 `PUT /api/config`，经与 CLI 配置命令相同的锁内 revision 校验原子写入链路保存用户配置，且任何配置变更都不触碰数据库。绑定 `0.0.0.0` 等公网地址会把用量数据与本地配置编辑暴露给局域网——请保持回环绑定（见 [`--addr`](cli.zh-CN.md#serve)）。

## 仪表盘页

内嵌页面为中英双语（按浏览器记忆）并带五套主题配色与深浅主题（按浏览器记忆，深浅默认跟随系统；均不进入配置脏状态）。仪表盘页全部由前端按数值行自绘：独立于输出列布局的七项核心指标；四个内置维度的两列卡片表（每卡底部固定合计、唯一尾部「其他（N 项）」行，另有构成环形图模式）；跟随选区范围的热力图——单日为 24 个小时格、2~7 天为日期×小时、8~31 天为日期×4 小时时段、32 天及以上为 GitHub 风格逐日日历，今天之后的格显示为空白（「尚未发生」）而非零值；用量趋势图；由 `query.subqueries` 生成的自定义视图表（附守恒保证的「其他组合」行）；保留原始标题的会话排行（信息按钮查看完整标题）。

自动刷新、手动刷新与切换区间共用一个请求入口和一套加载/成功/失败状态（过期响应不回写当前范围；失败整页清除旧统计并显示错误），间隔来自 `[refresh].dashboard_interval`。范围预设加无边界自定义日历（未来日期可提交并显示真实空结果；选区同一标签页跨刷新记忆）。

## 配置页

配置页编辑完整用户配置：对最近已保存快照实时差异计算脏状态——含两个查询刷新间隔（`[refresh] dashboard_interval`/`watch_interval`，下拉 10/20/30/60 秒加 1~3600 自定义值；未触碰的字段保持用户层原值，无关保存不会固化隐式默认）——revision 冲突（409）与校验失败（400）报告且绝不覆盖本地草稿、恢复为已保存配置需确认。

## HTTP 接口

| 接口 | 参数 | 返回 |
|------|------|------|
| `GET /api/meta` | — | 版本（由二进制按原样提供）、`min_date`/`max_date`（全库）、`data_through`、`last_collection`；后三项缺数据时为 `null` |
| `GET /api/dashboard` | `from`、`to`（`YYYY-MM-DD`；缺省为截至今天的 30 天；跨度至多 366 天；未来范围返回结构完整的全零载荷） | `data_through`（全库最新消息时间）、`last_collection`（全库最近成功采集时间），无值为 `null`，与统计结果同一读快照；统计区间、totals（整数，含 `active_days`）、`columns`（所配置输出列的指标 ID 序列——只影响明细表，不影响顶部七项）、`trend`（单日为 `hour` 粒度、2~62 天为 `day`、更长为 `week`；整数 `buckets` 携带桶边界键、`peak_index`/`peak_total`，全零区间 `empty=true`——不产生「峰值 0」）、四个固定维度数组（`client`/`model`/`provider`/`project`；至多 9 个独立行按 `total` 降序——同值按名称——加至多一行携带机器字段 `is_other`/`other_count` 的「其他」尾行，独立项＋尾行七项与 `totals` 逐项守恒）、带 `mode` 的 `heatmap` 选择展示桶形态：`day_hour`（1~7 天，每天本机时区 24 个 `hours`）、`day_block`（8~31 天，每天六个 4 小时 `blocks` 由后端求和）、`calendar`（32 天及以上，仅逐日 `total`），晚于今天的日期带 `future: true`——前端渲染为空白（「尚未发生」），已发生的零值日保持真实零格、`custom_views`（每个已配置子查询一项，按名称序：声明顺序的维度名与最终行——至多 19 个独立组合按 `total` 降序加至多一行 `is_other`「其他组合」，七项守恒）、前 20 条会话（原始标题）。全部截断、尾行合并与桶整理都在服务端于同一读快照内完成，单次刷新不可能混用快照，浏览器不再截断或重聚合 |
| `GET /api/config` | — | 可编辑配置模型（daemon、log、含数据来源路径的 clients、routers、provider aliases，以及解析后的 query 段——有效值，磁盘 query 段无法解析时附 `diagnostics`），与磁盘文件的 `revision`（文件尚不存在时为固定 sentinel）；`data_dir` 刻意不在模型中——数据目录迁移仍是仅 CLI 的确认操作 |
| `PUT /api/config` | 请求体 `{revision, config}`——`GET /api/config` 提供的草稿；`query` 段可省略以保持磁盘 query 段原样（页面仅在用户编辑过该段或明确恢复全部默认值时才回传，使无关区块的保存绝不改写解析失败的 query 段） | 成功返回规范化落盘草稿、新 `revision`、`changed`、`warnings`（文件已保存但自启同步等副作用失败时非空）与 `suggested_steps`；文件已被他处修改时返回 `409` 与当前 `revision`（保留本地草稿、不覆盖任何内容），校验失败（未知客户端/路径键、非法 query 定义、越界值）返回 `400`，写入失败返回 `500` |
| `GET /`、`GET /assets/…` | — | 内嵌 HTML 页面与静态资产（`Cache-Control: no-store`） |

- 错误统一为 JSON `{"error":{"message":"…"}}`：参数或草稿非法 `400`、资产不存在 `404`、revision 冲突 `409`、查询或写入失败 `500`。
- 数据面不设 CORS 头（按同源使用）、不与守护进程交互、绝不写数据库；唯一写路径是经共享 `configapp` 链路的 `PUT /api/config`。
- 维度行为原始整数，K/M/B 格式化交给前端；`provider` 行与查询视图一样应用 `[provider_aliases]`，配置变更（含别名与自定义视图）在下一次请求即生效。
