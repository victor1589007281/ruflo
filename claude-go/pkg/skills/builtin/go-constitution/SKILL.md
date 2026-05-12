# Go 代码宪法 v1.0

## 不可违背原则 (Hard Rules)
1. **错误处理**: 所有返回 `error` 的函数调用必须立即检查；禁止 `_, _ = fn()` 静默丢弃错误。
2. **并发安全**: 任何跨 goroutine 共享的可变状态必须使用 `sync.Mutex`、`sync.RWMutex`、`sync/atomic` 或 channel；禁止裸读写 `map`/`slice`。
3. **Context 传播**: 所有 I/O 操作、HTTP handler、goroutine 入口必须接受 `context.Context` 并向下传播；禁止 `context.Background()` 在业务代码中硬编码。
4. **资源释放**: 所有 `os.Open`、`net.Dial`、`http.Request.Body` 必须有 `defer Close()` 或等效保证。
5. **安全默认**: 禁止 `crypto/md5`/`sha1` 用于安全场景；禁止 `eval`/`exec` 拼接用户输入；禁止日志打印凭证。
6. **性能底线**: 禁止在热路径分配闭包捕获变量；禁止在循环内重复 `reflect.TypeOf`；优先使用 `sync.Pool` 管理高频临时对象。

## 协调原则 (Coordination Rules)
7. **契约优先**: 修改任何接口前，必须先更新契约定义，并通知所有实现者。
8. **最小修改**: 每次变更不得超过 200 行；超过必须拆分为子任务。
9. **测试锁死**: 任何功能变更必须伴随测试变更；修改测试通过逻辑来让测试通过是严重违规。
