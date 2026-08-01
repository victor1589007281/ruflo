package sync

// task_submitter.go —— "把一次同步交给 L2 任务服务执行" 的接口 (design/02 §3.5
// 通道表 sync 那一行: 「触发端点不变，执行体改提交 TaskService」)。
//
// ---------------------------------------------------------------------------
// 接口为什么定义在这里, 而不是在 pkg/agent 或 pkg/wiki
// ---------------------------------------------------------------------------
//
// 有两个消费方, 都不该看见对方:
//   - pkg/wiki 的 /sync/ima|weread|status/ 三个 HTTP 端点 (触发 + 查状态);
//   - pkg/feishu 的 registerSyncJobs (进程内 cron tick 触发, 要等结果好写日志)。
// 实现方 (pkg/synctask) 同时依赖 pkg/agent 与本包。若接口放在 pkg/agent, 两个
// 消费方就都得 import pkg/agent 才能声明字段; 若放在 pkg/wiki, pkg/feishu 要为了
// 一个接口 import pkg/wiki 的 HTTP 层。放在本包最窄: 两个消费方本来都已 import
// pkg/sync (Config/Job 都在这), 且本文件**不引入任何新依赖** —— 只有 context。
//
// ---------------------------------------------------------------------------
// 为什么返回 *Job 而不是任务 ID
// ---------------------------------------------------------------------------
//
// /sync/* 的响应形态是冻结契约 (POST → 202 {"jobId":...}; GET /sync/status/<id>
// → 200 <Job>), 下游 8+ 平台在按这个形状解析。让实现方直接吐 *Job 意味着
// "把 TaskRecord 映射成 Job" 这件事只发生在**一处**; 若接口只回 ID, 映射逻辑就
// 得在 wiki 的 handler 里再写一遍, 两份必然漂移 —— 而漂移的表现是下游拿到一个
// 缺字段的 Job, 那属于契约破坏。

import "context"

// TaskSubmitter 见文件头。
type TaskSubmitter interface {
	// SubmitSync 提交一次 source (ima/weread) 同步; 返回**已建档**的 Job。
	//
	// 幂等: 同一 source 已有未完成的同步任务时返回那一个, 而不是再起一次 ——
	// 两次 RunSync 并发跑同一个 source 会同时读改同一份 index.json, 后写的那份
	// 覆盖前一份 (LoadIndex/SaveIndex 之间没有锁)。这是改造顺带修掉的一个
	// 既有缺陷: 现状连点两下 /sync/ima 就会双跑。
	SubmitSync(source string) (*Job, error)

	// SyncJob 查一次同步的当前状态; 第二个返回值 = 是否存在。
	SyncJob(jobID string) (*Job, bool)

	// WaitSync 阻塞到终态 (或 ctx 取消)。供 cron tick 这类"要等结果写日志"的
	// 调用方使用; HTTP 触发端点不用它 (那边是 202 立即返回)。
	WaitSync(ctx context.Context, jobID string) (*Job, error)
}
