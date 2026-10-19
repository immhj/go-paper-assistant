# 单实例运行，arXiv 限速由 paperscraper sidecar 统一管理

论文搜索通过独立 Python sidecar 使用 `jannisborn/paperscraper` v1.0.0。第一版只启用 arXiv API 后端；sidecar 使用全局锁串行查询，并强制 `delay_seconds >= 3`，避免多个 Go 请求绕过 arXiv 的调用间隔。

限速放在 sidecar 进程内存里，不用 Redis。当前 Go 服务单实例、QQ 使用 WebSocket 长连接，Python sidecar 也只运行一个实例；这两个前提使进程内限速足够。若未来改为 Webhook 并横向扩展，必须把 source rate limit 迁到共享协调层，不能简单增加 sidecar 副本。

Go adapter 只负责 HTTP 超时与 429/5xx 的有界重试；数据源特有的查询节奏属于 Python adapter 的实现细节，不泄漏进 service。

未来的订阅批处理必须低于交互请求优先级。在真正实现订阅前不为此引入 Redis 或持久队列。
