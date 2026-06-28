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
