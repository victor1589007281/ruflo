package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// PIDFile 持久化的守护进程描述。
type PIDFile struct {
	PID       int       `json:"pid"`
	Port      int       `json:"port"`
	Addr      string    `json:"addr"`
	StartedAt time.Time `json:"startedAt"`
	StateDir  string    `json:"stateDir"`
	LogFile   string    `json:"logFile,omitempty"`
}

// DaemonDir 返回存放 pid / 日志的目录。
// 优先使用 <stateDir>/.dashboard/ (隐藏), 若 stateDir 不可写则退化到临时目录。
func DaemonDir(stateDir string) string {
	d := filepath.Join(stateDir, ".dashboard")
	if err := os.MkdirAll(d, 0o755); err == nil {
		return d
	}
	return filepath.Join(os.TempDir(), "claude-go-dashboard")
}

// PIDFilePath 返回 pid 文件路径。
func PIDFilePath(stateDir string) string {
	return filepath.Join(DaemonDir(stateDir), "dashboard.pid")
}

// LogFilePath 返回后台日志文件路径。
func LogFilePath(stateDir string) string {
	return filepath.Join(DaemonDir(stateDir), "dashboard.log")
}

// WritePIDFile 落盘 pid 描述。
func WritePIDFile(stateDir string, p PIDFile) error {
	_ = os.MkdirAll(DaemonDir(stateDir), 0o755)
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(PIDFilePath(stateDir), data, 0o644)
}

// ReadPIDFile 读取, 不存在返回 (zero, nil)。
func ReadPIDFile(stateDir string) (PIDFile, error) {
	var p PIDFile
	data, err := os.ReadFile(PIDFilePath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return p, nil
		}
		return p, err
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, err
	}
	return p, nil
}

// RemovePIDFile 安静删除。
func RemovePIDFile(stateDir string) {
	_ = os.Remove(PIDFilePath(stateDir))
}

// IsProcessAlive 判断 PID 是否还活着 (Unix: kill -0)。
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// QueryStatus 查询后台进程状态。
func QueryStatus(stateDir string) (DaemonStatus, error) {
	p, err := ReadPIDFile(stateDir)
	if err != nil {
		return DaemonStatus{StateDir: stateDir}, err
	}
	st := DaemonStatus{
		PID:       p.PID,
		Port:      p.Port,
		Addr:      p.Addr,
		StartedAt: p.StartedAt,
		StateDir:  p.StateDir,
		LogFile:   p.LogFile,
	}
	if st.StateDir == "" {
		st.StateDir = stateDir
	}
	if p.PID > 0 && IsProcessAlive(p.PID) {
		st.Running = true
		if p.Addr != "" {
			st.URL = "http://" + p.Addr
		}
		if !p.StartedAt.IsZero() {
			st.Uptime = fmtDuration(time.Since(p.StartedAt))
		}
	}
	return st, nil
}

// StopDaemon 尝试优雅停止 (SIGTERM), 失败则 SIGKILL。
// 成功后清理 pid 文件。
func StopDaemon(stateDir string) (DaemonStatus, error) {
	st, _ := QueryStatus(stateDir)
	if !st.Running || st.PID <= 0 {
		// 清理僵尸 pid 文件
		RemovePIDFile(stateDir)
		st.Running = false
		return st, nil
	}
	proc, err := os.FindProcess(st.PID)
	if err != nil {
		return st, err
	}
	_ = proc.Signal(syscall.SIGTERM)
	for i := 0; i < 30; i++ { // 等待最多 3s
		if !IsProcessAlive(st.PID) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if IsProcessAlive(st.PID) {
		_ = proc.Signal(syscall.SIGKILL)
		for i := 0; i < 10; i++ {
			if !IsProcessAlive(st.PID) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if IsProcessAlive(st.PID) {
		return st, fmt.Errorf("进程 %d 仍在运行, 请手工 kill", st.PID)
	}
	RemovePIDFile(stateDir)
	st.Running = false
	return st, nil
}

// FindFreePort 在给定端口尝试顺序查找可用端口。
func FindFreePort(preferred int) int {
	if preferred <= 0 {
		preferred = 7777
	}
	for port := preferred; port < preferred+50; port++ {
		ln, err := tryListen(port)
		if err == nil {
			_ = ln.Close()
			return port
		}
	}
	return preferred
}
