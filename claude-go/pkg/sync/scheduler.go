package sync

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Scheduler 是极简的 cron 调度器，支持 5 字段 cron（分 时 日 月 周）。
type Scheduler struct {
	cfg     Config
	jobs    map[string]*scheduledJob
	mu      sync.Mutex
	ticker  *time.Ticker
	stop    chan struct{}
	wg      sync.WaitGroup
	running map[string]*Job
}

type scheduledJob struct {
	name     string
	cron     string
	fn       func(ctx context.Context) error
	lastRun  time.Time
	schedule cronSchedule
}

type cronSchedule struct {
	minute      map[int]bool
	hour        map[int]bool
	dayOfMonth  map[int]bool
	month       map[int]bool
	dayOfWeek   map[int]bool
}

// NewScheduler 创建同步调度器。
func NewScheduler(cfg Config) *Scheduler {
	return &Scheduler{
		cfg:     cfg,
		jobs:    make(map[string]*scheduledJob),
		running: make(map[string]*Job),
	}
}

// Config 返回调度器使用的配置。
func (s *Scheduler) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// Register 注册一个定时同步任务。
func (s *Scheduler) Register(name, cron string, fn func(ctx context.Context) error) error {
	sched, err := parseCron(cron)
	if err != nil {
		return fmt.Errorf("解析 cron %q 失败: %w", cron, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[name] = &scheduledJob{
		name:     name,
		cron:     cron,
		fn:       fn,
		schedule: sched,
	}
	return nil
}

// Start 启动调度器。
func (s *Scheduler) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ticker != nil {
		return
	}
	ticker := time.NewTicker(time.Minute)
	stop := make(chan struct{})
	s.ticker = ticker
	s.stop = stop
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			select {
			case <-stop:
				ticker.Stop()
				return
			case now := <-ticker.C:
				s.check(now)
			}
		}
	}()
}

// Stop 停止调度器。
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if s.ticker != nil {
		close(s.stop)
		s.ticker = nil
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// check 在每分钟的 tick 上挑出到点的任务并异步触发一次。
//
// 算法解读：
//
//	原理：先把任务列表在锁内**快照**成切片再在锁外遍历 —— 任务函数可能跑很久,
//	      持锁遍历会让 Register / Stop 全部排队。
//	关键不变量：去重用 lastRun 与**截断到分钟**的 now 比较, 保证同一个分钟窗口内
//	      无论 tick 抖动几次都只触发一次。注意 lastRun 的读写在锁**外** (快照后),
//	      即依赖"每分钟只有一个 goroutine 在跑 check"这一前提 —— ticker 单消费者
//	      保证了它, 但它是隐式契约而非显式锁。
//	代价与边界：触发是 fire-and-forget, 每次 go 一个 30 分钟超时的 goroutine;
//	      同一任务若上次还没跑完就到点, 会并发再起一个 (没有"上一个还在跑就跳过")。
//	设计权衡：任务超时固定 30 分钟写死在这里, 不随任务配置变化。
func (s *Scheduler) check(now time.Time) {
	s.mu.Lock()
	jobs := make([]*scheduledJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, j)
	}
	s.mu.Unlock()

	for _, j := range jobs {
		if !match(j.schedule, now) {
			continue
		}
		truncated := now.Truncate(time.Minute)
		if j.lastRun.Equal(truncated) {
			continue
		}
		// 去重标记: 同一个分钟窗口内只触发一次。此处的读写在锁外 —— 依赖"每分钟
		// 只有一个 goroutine 在跑 check" (ticker 单消费者) 这个隐式契约。
		j.lastRun = truncated
		go func(j *scheduledJob) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			if err := j.fn(ctx); err != nil {
				log.Printf("[sync] 定时任务 %s 失败: %v", j.name, err)
			}
		}(j)
	}
}

// RunNow 立即执行一次指定任务并返回任务记录。
func (s *Scheduler) RunNow(name string, fn func(ctx context.Context) error) *Job {
	job := &Job{
		ID:        fmt.Sprintf("%s-%d", name, time.Now().UnixNano()),
		Source:    name,
		Status:    JobPending,
		StartedAt: time.Now().UTC(),
	}
	s.mu.Lock()
	s.running[job.ID] = job
	s.mu.Unlock()

	go func() {
		s.mu.Lock()
		job.Status = JobRunning
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		err := fn(ctx)
		now := time.Now().UTC()
		job.FinishedAt = &now
		s.mu.Lock()
		if err != nil {
			job.Status = JobFailed
			job.Message = err.Error()
		} else {
			job.Status = JobCompleted
			job.Message = "ok"
		}
		s.mu.Unlock()
	}()

	return job
}

// JobStatusByID 查询任务状态。
func (s *Scheduler) JobStatusByID(id string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.running[id]
	return j, ok
}

// parseCron 解析 5 字段 cron 表达式 (分 时 日 月 周), 每段展开成"取值集合"。
// 段数不是 5 个直接报错 —— 不支持秒字段与 @daily 之类的别名。
func parseCron(expr string) (cronSchedule, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return cronSchedule{}, fmt.Errorf("cron 表达式需要 5 个字段")
	}
	sched := cronSchedule{}
	var err error
	if sched.minute, err = parseCronField(fields[0], 0, 59); err != nil {
		return sched, fmt.Errorf("minute: %w", err)
	}
	if sched.hour, err = parseCronField(fields[1], 0, 23); err != nil {
		return sched, fmt.Errorf("hour: %w", err)
	}
	if sched.dayOfMonth, err = parseCronField(fields[2], 1, 31); err != nil {
		return sched, fmt.Errorf("day of month: %w", err)
	}
	if sched.month, err = parseCronField(fields[3], 1, 12); err != nil {
		return sched, fmt.Errorf("month: %w", err)
	}
	if sched.dayOfWeek, err = parseCronField(fields[4], 0, 6); err != nil {
		return sched, fmt.Errorf("day of week: %w", err)
	}
	return sched, nil
}

// parseCronField 把单个 cron 字段展开成取值集合, 支持 * / a,b,c / a-b / a/n / */n。
//
// 注意：范围校验**只做在"单个数值"这一条分支上** —— `a-b` 的上下界与 `a/n` 的起点
// 都不校验, 越界的值会被静默塞进集合 (永不匹配, 表现为"任务不触发"而不是报错)。
// 更糟的是 `*/0`: step 为 0 时下面的 for 循环 `i += 0` 永不推进, 会死循环并不断往
// map 里写同一个键 (占满 CPU / 内存)。此处按铁律只记录不改。
func parseCronField(field string, min, max int) (map[int]bool, error) {
	out := make(map[int]bool)
	if field == "*" {
		for i := min; i <= max; i++ {
			out[i] = true
		}
		return out, nil
	}
	for _, p := range strings.Split(field, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.Contains(p, "/") {
			parts := strings.SplitN(p, "/", 2)
			start := min
			if parts[0] != "*" {
				v, err := strconv.Atoi(parts[0])
				if err != nil {
					return nil, err
				}
				start = v
			}
			step, err := strconv.Atoi(parts[1])
			if err != nil {
				return nil, err
			}
			for i := start; i <= max; i += step {
				out[i] = true
			}
			continue
		}
		if strings.Contains(p, "-") {
			parts := strings.SplitN(p, "-", 2)
			lo, err := strconv.Atoi(parts[0])
			if err != nil {
				return nil, err
			}
			hi, err := strconv.Atoi(parts[1])
			if err != nil {
				return nil, err
			}
			for i := lo; i <= hi; i++ {
				out[i] = true
			}
			continue
		}
		v, err := strconv.Atoi(p)
		if err != nil {
			return nil, err
		}
		if v < min || v > max {
			return nil, fmt.Errorf("值 %d 超出范围 [%d,%d]", v, min, max)
		}
		out[v] = true
	}
	return out, nil
}

// match 判断某个时刻是否命中该 cron 调度。
//
// 算法解读：
//
//	原理：分 / 时 / 月三段是**与**关系, 命中集合即通过。日 (day-of-month) 与周
//	      (day-of-week) 沿用 Vixie cron 的规则: 两者都写了就取**或**, 只写了一个
//	      就只看那一个。
//	关键不变量：代码用**集合大小**反推"这一段是不是 *" —— dayOfMonth 满集恒为 31 个
//	      元素, dayOfWeek 满集恒为 7 个。这个反推依赖 parseCronField 对 `*` 的展开
//	      方式 (逐值填满 [min,max]), 与字段本身的语义无关。
//	代价与边界：写 `1-31` 或 `0-6` 这种"人工枚举的全集"同样得到 31 / 7 个元素, 会被
//	      误判成 `*`, 于是另一段的限制被丢掉 —— 判据是"像 *"而不是"是 *"。
//	设计权衡：宁可保留反推也不引入额外的"是否为 *"标志位, 因为 cronSchedule 的五个
//	      字段被当成同质的集合使用; 代价就是上面那条边界。
func match(s cronSchedule, t time.Time) bool {
	if !s.minute[t.Minute()] {
		return false
	}
	if !s.hour[t.Hour()] {
		return false
	}
	if !s.month[int(t.Month())] {
		return false
	}
	dayMatch := s.dayOfMonth[t.Day()]
	weekMatch := s.dayOfWeek[int(t.Weekday())]
	// 与标准 cron 一致：同时指定 day-of-month 和 day-of-week 时取或
	// 下面四段是 Vixie cron 的"日/周"规则, 判据是集合大小: 满集 31 = 日字段为 *,
	// 满集 7 = 周字段为 *。两者都不是 * → 取或; 只有一个是 * → 只看另一个。
	if len(s.dayOfMonth) == 31 && len(s.dayOfWeek) == 7 {
		return dayMatch || weekMatch
	}
	if len(s.dayOfMonth) == 31 {
		return weekMatch
	}
	if len(s.dayOfWeek) == 7 {
		return dayMatch
	}
	return dayMatch || weekMatch
}
