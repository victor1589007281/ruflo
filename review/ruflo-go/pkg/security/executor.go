package security

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// SafeExecutor runs allowlisted binaries without a shell.
type SafeExecutor struct {
	allowlist    map[string]string // name -> absolute path
	blockedArgRe []*regexp.Regexp
}

// NewSafeExecutor builds an executor with a command allowlist (names without path use PATH lookup once at registration).
func NewSafeExecutor(allowed map[string]string) *SafeExecutor {
	if allowed == nil {
		allowed = map[string]string{}
	}
	se := &SafeExecutor{
		allowlist: allowed,
		blockedArgRe: []*regexp.Regexp{
			regexp.MustCompile(`(?i)(rm\s+-rf\b|mkfs\b|dd\s+if=)`),
			regexp.MustCompile(`[;&|$\x60]`),
		},
	}
	return se
}

// RegisterAllow maps a logical name to an absolute executable path.
func (s *SafeExecutor) RegisterAllow(name, absPath string) {
	if s.allowlist == nil {
		s.allowlist = make(map[string]string)
	}
	s.allowlist[name] = absPath
}

// Run executes name with args if allowed; never invokes a shell.
func (s *SafeExecutor) Run(ctx context.Context, name string, args []string) ([]byte, error) {
	if s == nil {
		return nil, errors.New("security: nil executor")
	}
	exe, ok := s.allowlist[name]
	if !ok {
		return nil, fmt.Errorf("security: command not allowlisted: %s", name)
	}
	exe = filepath.Clean(exe)
	for _, a := range args {
		if s.argBlocked(a) {
			return nil, fmt.Errorf("security: blocked argument pattern")
		}
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = minimalEnv()
	return cmd.Output()
}

func (s *SafeExecutor) argBlocked(a string) bool {
	for _, re := range s.blockedArgRe {
		if re.MatchString(a) {
			return true
		}
	}
	return false
}

func minimalEnv() []string {
	return []string{
		"PATH=" + strings.Join([]string{"/usr/bin", "/bin", "/usr/local/bin"}, string(filepath.ListSeparator)),
		"LANG=C.UTF-8",
	}
}
