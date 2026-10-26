// Package k8s 用只读 kubectl 采集集群证据：节点状态、Pod 失败模式、Warning
// 事件、PVC、工作负载副本数。
//
// 安全边界：只调用固定只读动词（get），context 必须是 kubeconfig 里已存在的
// 名字，namespace 只允许合法 DNS label；模型不能提供任意 kubectl 参数，也不能
// 通过 shell 拼接命令。kubeconfig 由运维提供，工具不读取/输出凭证内容。
package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/ops/evidence"
)

const (
	defaultWindow     = 30 * time.Minute
	defaultCmdTimeout = 12 * time.Second
	maxCommandBytes   = 4 << 20
)

// Config 是 K8s 采集参数。
type Config struct {
	Context      string
	Namespace    string
	Window       time.Duration
	QueryTimeout time.Duration
	Now          func() time.Time
	Kubectl      string
}

// Collector 采集一个集群上下文。
type Collector struct {
	cfg Config
	now func() time.Time
}

var nsPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// New 校验 context/namespace 并定位 kubectl。
func New(cfg Config) (*Collector, error) {
	cfg.Context = strings.TrimSpace(cfg.Context)
	cfg.Namespace = strings.TrimSpace(cfg.Namespace)
	if cfg.Context == "" {
		return nil, fmt.Errorf("context is required（用 kubectl config get-contexts 查看可用 context）")
	}
	if cfg.Namespace != "" && (!nsPattern.MatchString(cfg.Namespace) || len(cfg.Namespace) > 63) {
		return nil, fmt.Errorf("namespace %q 非法：只允许小写字母/数字/连字符，长度 1-63", cfg.Namespace)
	}
	if cfg.Window <= 0 {
		cfg.Window = defaultWindow
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = defaultCmdTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Kubectl == "" {
		cfg.Kubectl = strings.TrimSpace(os.Getenv("CLAUDE_GO_KUBECTL"))
		if cfg.Kubectl == "" {
			cfg.Kubectl = "kubectl"
		}
	}
	if _, err := exec.LookPath(cfg.Kubectl); err != nil {
		return nil, fmt.Errorf("找不到 kubectl（%s）: %w", cfg.Kubectl, err)
	}
	contexts, err := listContexts(cfg.Kubectl, cfg.QueryTimeout)
	if err != nil {
		return nil, fmt.Errorf("读取 kubeconfig contexts 失败: %w", err)
	}
	found := false
	for _, c := range contexts {
		if c == cfg.Context {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("context %q 不在 kubeconfig；可用: %s", cfg.Context, strings.Join(contexts, ", "))
	}
	return &Collector{cfg: cfg, now: cfg.Now}, nil
}

func listContexts(kubectl string, timeout time.Duration) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, kubectl, "config", "get-contexts", "-o", "name")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			names = append(names, s)
		}
	}
	return names, nil
}

type containerState struct {
	Waiting *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"waiting"`
	Running *struct {
		StartedAt time.Time `json:"startedAt"`
	} `json:"running"`
	Terminated *struct {
		ExitCode   int       `json:"exitCode"`
		Reason     string    `json:"reason"`
		Message    string    `json:"message"`
		FinishedAt time.Time `json:"finishedAt"`
	} `json:"terminated"`
}

type containerInfo struct {
	Name         string         `json:"name"`
	Image        string         `json:"image"`
	Ready        bool           `json:"ready"`
	RestartCount int            `json:"restartCount"`
	State        containerState `json:"state"`
	LastState    containerState `json:"lastState"`
}

type nodeInfo struct {
	Name       string
	Ready      bool
	Conditions map[string]string
	Pressure   []string
	Kubelet    string
	OSImage    string
	Capacity   map[string]string
	Alloc      map[string]string
}

type podInfo struct {
	Namespace      string
	Name           string
	Node           string
	Phase          string
	Reason         string
	Message        string
	Created        time.Time
	Containers     []containerInfo
	InitContainers []containerInfo
}

type eventInfo struct {
	Namespace string
	Kind      string
	Name      string
	Reason    string
	Message   string
	Type      string
	Count     int
	Last      time.Time
}

type pvcInfo struct {
	Namespace    string
	Name         string
	Phase        string
	StorageClass string
	Capacity     string
}

type workloadInfo struct {
	Kind        string
	Namespace   string
	Name        string
	Desired     int
	Ready       int
	Available   int
	Unavailable int
}

type snapshot struct {
	context   string
	namespace string
	now       time.Time
	window    time.Duration

	nodes      []nodeInfo
	nodesValid bool
	nodesNote  string

	pods      []podInfo
	podsValid bool
	podsNote  string

	events      []eventInfo
	eventsValid bool
	eventsNote  string

	pvcs      []pvcInfo
	pvcsValid bool
	pvcsNote  string

	workloads      []workloadInfo
	workloadsValid bool
	workloadsNote  string
}

// Collect 采集一次集群快照并生成证据包。
func (c *Collector) Collect(ctx context.Context) (*evidence.Bundle, error) {
	s := &snapshot{context: c.cfg.Context, namespace: c.cfg.Namespace, now: c.now(), window: c.cfg.Window}

	if out, err := c.get(ctx, "nodes", false, nil); err != nil {
		s.nodesNote = "kubectl get nodes 失败: " + err.Error()
	} else if nodes, err := parseNodes(out); err != nil {
		s.nodesNote = "nodes JSON 解析失败: " + err.Error()
	} else {
		s.nodes, s.nodesValid = nodes, true
	}

	if out, err := c.get(ctx, "pods", c.cfg.Namespace == "", nil); err != nil {
		s.podsNote = "kubectl get pods 失败: " + err.Error()
	} else if pods, err := parsePods(out); err != nil {
		s.podsNote = "pods JSON 解析失败: " + err.Error()
	} else {
		s.pods, s.podsValid = pods, true
	}

	if out, err := c.get(ctx, "events", c.cfg.Namespace == "", []string{"--field-selector=type=Warning"}); err != nil {
		s.eventsNote = "kubectl get events 失败: " + err.Error()
	} else if events, err := parseEvents(out); err != nil {
		s.eventsNote = "events JSON 解析失败: " + err.Error()
	} else {
		s.events, s.eventsValid = events, true
	}

	if out, err := c.get(ctx, "pvc", c.cfg.Namespace == "", nil); err != nil {
		s.pvcsNote = "kubectl get pvc 失败: " + err.Error()
	} else if pvcs, err := parsePVCs(out); err != nil {
		s.pvcsNote = "pvc JSON 解析失败: " + err.Error()
	} else {
		s.pvcs, s.pvcsValid = pvcs, true
	}

	for _, res := range []string{"deployments", "statefulsets", "daemonsets"} {
		out, err := c.get(ctx, res, c.cfg.Namespace == "", nil)
		if err != nil {
			s.workloadsNote = fmt.Sprintf("kubectl get %s 失败: %v", res, err)
			continue
		}
		w, err := parseWorkloads(res, out)
		if err != nil {
			s.workloadsNote = fmt.Sprintf("%s JSON 解析失败: %v", res, err)
			continue
		}
		s.workloads = append(s.workloads, w...)
		s.workloadsValid = true
	}

	b := c.buildBundle(s)
	analyze(b, s)
	if err := b.Validate(); err != nil {
		return b, err
	}
	return b, nil
}

func (c *Collector) get(ctx context.Context, resource string, allNamespaces bool, extra []string) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, c.cfg.QueryTimeout)
	defer cancel()
	args := []string{"--context", c.cfg.Context, "--request-timeout=10s", "get", resource}
	if len(extra) > 0 {
		args = append(args, extra...)
	}
	switch {
	case allNamespaces:
		args = append(args, "-A")
	case c.cfg.Namespace != "":
		args = append(args, "-n", c.cfg.Namespace)
	}
	args = append(args, "-o", "json")
	cmd := exec.CommandContext(runCtx, c.cfg.Kubectl, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	stdout := &capBuffer{limit: maxCommandBytes}
	stderr := &capBuffer{limit: 4096}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("%v: %s", err, msg)
		}
		return nil, err
	}
	if stdout.truncated {
		return nil, fmt.Errorf("kubectl 输出超过 %d 字节被截断；请指定 namespace 缩小范围", maxCommandBytes)
	}
	return stdout.buf.Bytes(), nil
}

type capBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	remain := b.limit - b.buf.Len()
	if remain <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remain {
		b.buf.Write(p[:remain])
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *capBuffer) String() string { return b.buf.String() }

// ---- JSON 解析 --------------------------------------------------------------

func parseNodes(raw []byte) ([]nodeInfo, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type    string `json:"type"`
					Status  string `json:"status"`
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"conditions"`
				Capacity    map[string]string `json:"capacity"`
				Allocatable map[string]string `json:"allocatable"`
				NodeInfo    struct {
					KubeletVersion string `json:"kubeletVersion"`
					OSImage        string `json:"osImage"`
				} `json:"nodeInfo"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := make([]nodeInfo, 0, len(list.Items))
	for _, item := range list.Items {
		n := nodeInfo{
			Name: item.Metadata.Name, Conditions: map[string]string{},
			Kubelet: item.Status.NodeInfo.KubeletVersion, OSImage: item.Status.NodeInfo.OSImage,
			Capacity: item.Status.Capacity, Alloc: item.Status.Allocatable,
		}
		for _, cond := range item.Status.Conditions {
			n.Conditions[cond.Type] = cond.Status
			if cond.Status == "True" {
				switch cond.Type {
				case "Ready":
					n.Ready = true
				case "MemoryPressure", "DiskPressure", "PIDPressure", "NetworkUnavailable":
					n.Pressure = append(n.Pressure, cond.Type)
				}
			}
		}
		out = append(out, n)
	}
	return out, nil
}

func parsePods(raw []byte) ([]podInfo, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string    `json:"name"`
				Namespace         string    `json:"namespace"`
				CreationTimestamp time.Time `json:"creationTimestamp"`
			} `json:"metadata"`
			Spec struct {
				NodeName       string          `json:"nodeName"`
				Containers     []containerInfo `json:"containers"`
				InitContainers []containerInfo `json:"initContainers"`
			} `json:"spec"`
			Status struct {
				Phase                 string          `json:"phase"`
				Reason                string          `json:"reason"`
				Message               string          `json:"message"`
				ContainerStatuses     []containerInfo `json:"containerStatuses"`
				InitContainerStatuses []containerInfo `json:"initContainerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	// spec.containers 只有 name/image；status.containerStatuses 才有状态。
	// 以 status 为准，spec 仅用于补齐缺失的容器名。
	out := make([]podInfo, 0, len(list.Items))
	for _, item := range list.Items {
		p := podInfo{
			Namespace: item.Metadata.Namespace, Name: item.Metadata.Name,
			Node: item.Spec.NodeName, Phase: item.Status.Phase,
			Reason: item.Status.Reason, Message: item.Status.Message,
			Created:        item.Metadata.CreationTimestamp,
			Containers:     item.Status.ContainerStatuses,
			InitContainers: item.Status.InitContainerStatuses,
		}
		out = append(out, p)
	}
	return out, nil
}

func parseEvents(raw []byte) ([]eventInfo, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Type      string    `json:"type"`
			Reason    string    `json:"reason"`
			Message   string    `json:"message"`
			Count     int       `json:"count"`
			LastTime  time.Time `json:"lastTimestamp"`
			EventTime time.Time `json:"eventTime"`
			Involved  struct {
				Kind      string `json:"kind"`
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
				FieldPath string `json:"fieldPath"`
			} `json:"involvedObject"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := make([]eventInfo, 0, len(list.Items))
	for _, item := range list.Items {
		last := item.LastTime
		if last.IsZero() {
			last = item.EventTime
		}
		ns := item.Metadata.Namespace
		if ns == "" {
			ns = item.Involved.Namespace
		}
		out = append(out, eventInfo{
			Namespace: ns, Kind: item.Involved.Kind, Name: item.Involved.Name,
			Reason: item.Reason, Message: item.Message, Type: item.Type,
			Count: item.Count, Last: last,
		})
	}
	return out, nil
}

func parsePVCs(raw []byte) ([]pvcInfo, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				StorageClassName *string `json:"storageClassName"`
			} `json:"spec"`
			Status struct {
				Phase    string            `json:"phase"`
				Capacity map[string]string `json:"capacity"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := make([]pvcInfo, 0, len(list.Items))
	for _, item := range list.Items {
		p := pvcInfo{Namespace: item.Metadata.Namespace, Name: item.Metadata.Name, Phase: item.Status.Phase}
		if item.Spec.StorageClassName != nil {
			p.StorageClass = *item.Spec.StorageClassName
		}
		if v, ok := item.Status.Capacity["storage"]; ok {
			p.Capacity = v
		}
		out = append(out, p)
	}
	return out, nil
}

func parseWorkloads(kind string, raw []byte) ([]workloadInfo, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				Replicas *int `json:"replicas"`
			} `json:"spec"`
			Status struct {
				Replicas               int `json:"replicas"`
				ReadyReplicas          int `json:"readyReplicas"`
				AvailableReplicas      int `json:"availableReplicas"`
				UnavailableReplicas    int `json:"unavailableReplicas"`
				DesiredNumberScheduled int `json:"desiredNumberScheduled"`
				NumberReady            int `json:"numberReady"`
				NumberUnavailable      int `json:"numberUnavailable"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := make([]workloadInfo, 0, len(list.Items))
	for _, item := range list.Items {
		w := workloadInfo{Kind: kind, Namespace: item.Metadata.Namespace, Name: item.Metadata.Name}
		switch kind {
		case "daemonsets":
			w.Kind = "DaemonSet"
			w.Desired = item.Status.DesiredNumberScheduled
			w.Ready = item.Status.NumberReady
			w.Unavailable = item.Status.NumberUnavailable
		case "statefulsets":
			w.Kind = "StatefulSet"
			if item.Spec.Replicas != nil {
				w.Desired = *item.Spec.Replicas
			} else {
				w.Desired = item.Status.Replicas
			}
			w.Ready = item.Status.ReadyReplicas
			w.Available = item.Status.AvailableReplicas
		default:
			w.Kind = "Deployment"
			if item.Spec.Replicas != nil {
				w.Desired = *item.Spec.Replicas
			} else {
				w.Desired = item.Status.Replicas
			}
			w.Ready = item.Status.ReadyReplicas
			w.Available = item.Status.AvailableReplicas
			w.Unavailable = item.Status.UnavailableReplicas
		}
		out = append(out, w)
	}
	return out, nil
}

// ---- 派生读数 ---------------------------------------------------------------

func (s *snapshot) podIssues() []podIssue {
	var out []podIssue
	for _, p := range s.pods {
		for _, c := range append(append([]containerInfo{}, p.InitContainers...), p.Containers...) {
			if c.State.Waiting != nil && isFailureReason(c.State.Waiting.Reason) {
				out = append(out, podIssue{
					Namespace: p.Namespace, Pod: p.Name, Node: p.Node, Container: c.Name,
					Reason: c.State.Waiting.Reason, Message: c.State.Waiting.Message,
					Restarts: c.RestartCount, Active: true, LastTermination: lastReason(c),
					FinishedAt: lastFinished(c),
				})
				continue
			}
			if c.State.Terminated != nil && c.State.Terminated.ExitCode != 0 {
				out = append(out, podIssue{
					Namespace: p.Namespace, Pod: p.Name, Node: p.Node, Container: c.Name,
					Reason: c.State.Terminated.Reason, Message: c.State.Terminated.Message,
					Restarts: c.RestartCount, Active: true, LastTermination: c.State.Terminated.Reason,
					FinishedAt: c.State.Terminated.FinishedAt,
				})
				continue
			}
			if c.RestartCount > 0 && lastReason(c) != "" {
				out = append(out, podIssue{
					Namespace: p.Namespace, Pod: p.Name, Node: p.Node, Container: c.Name,
					Reason: lastReason(c), Restarts: c.RestartCount,
					LastTermination: lastReason(c), FinishedAt: lastFinished(c), Active: false,
				})
			}
		}
	}
	return out
}

type podIssue struct {
	Namespace       string
	Pod             string
	Node            string
	Container       string
	Reason          string
	Message         string
	Restarts        int
	Active          bool
	LastTermination string
	FinishedAt      time.Time
}

func isFailureReason(reason string) bool {
	switch reason {
	case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerError",
		"CreateContainerConfigError", "RunContainerError", "InvalidImageName":
		return true
	}
	return false
}

func lastReason(c containerInfo) string {
	if c.LastState.Terminated != nil {
		return c.LastState.Terminated.Reason
	}
	return ""
}

func lastFinished(c containerInfo) time.Time {
	if c.LastState.Terminated != nil {
		return c.LastState.Terminated.FinishedAt
	}
	return time.Time{}
}

func (s *snapshot) nodeByName(name string) *nodeInfo {
	for i := range s.nodes {
		if s.nodes[i].Name == name {
			return &s.nodes[i]
		}
	}
	return nil
}

func eventMatches(ev eventInfo, kind, name, ns string) bool {
	if !strings.EqualFold(ev.Kind, kind) || ev.Name != name {
		return false
	}
	return ns == "" || ev.Namespace == ns
}

func (s *snapshot) eventFor(kind, name, ns string, reasons ...string) *eventInfo {
	for i := range s.events {
		ev := &s.events[i]
		if !eventMatches(*ev, kind, name, ns) {
			continue
		}
		if len(reasons) == 0 {
			return ev
		}
		for _, r := range reasons {
			if strings.EqualFold(ev.Reason, r) {
				return ev
			}
		}
	}
	return nil
}

func (s *snapshot) recentEvents(window time.Duration) []eventInfo {
	var out []eventInfo
	for _, ev := range s.events {
		if ev.Last.IsZero() || s.now.Sub(ev.Last) <= window {
			out = append(out, ev)
		}
	}
	return out
}

func topEventReasons(events []eventInfo, n int) string {
	counts := map[string]int{}
	for _, ev := range events {
		counts[ev.Reason] += max(ev.Count, 1)
	}
	type kv struct {
		k string
		n int
	}
	var list []kv
	for k, v := range counts {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].k < list[j].k
	})
	var parts []string
	for i, x := range list {
		if i >= n {
			break
		}
		parts = append(parts, fmt.Sprintf("%s×%d", x.k, x.n))
	}
	return strings.Join(parts, ",")
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
